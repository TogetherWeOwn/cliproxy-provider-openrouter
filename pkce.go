package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Real PKCE login for OpenRouter, the one provider with browser OAuth.
//
// The management UI shows a "Start OpenRouter Login" card for every plugin
// with an AuthProvider. The flow copies the official CLIProxy OAuth providers
// (Codex, Claude, ...) with no host change:
//
//  1. StartLogin mints a PKCE verifier/challenge pair plus a random state and
//     returns the OpenRouter approval URL itself
//     (openrouter.ai/auth?callback_url=<host loopback base>?state=... plus the
//     S256 challenge). The panel opens that URL in its standard OAuth dialog.
//  2. The operator approves; OpenRouter redirects to the loopback callback,
//     which a remote browser cannot load. The operator copies the full
//     callback URL from their address bar into the panel's callback box, and
//     the panel POSTs it to the host's own OAuth callback. The host extracts
//     the one-time code and publishes it atomically to
//     .oauth-openrouter-<state>.oauth in the auth directory.
//  3. PollLogin reads that file, exchanges the code for an API key with
//     POST /api/v1/auth/keys (code + verifier, S256), deletes the file, and
//     runs the provider's full ParseAuth pipeline, including the
//     duplicate-key guard, so the login faces the same checks as an
//     uploaded auth file. Exactly one poller runs the exchange; a
//     concurrent second poller stays pending.
//
// The verifier never leaves the plugin process except in the exchange POST
// body; the code is single-use and the file is removed once read. There is no
// plugin-served page or submit endpoint: the panel's standard dialog and the
// host's OAuth callback do that work.
const (
	oauthAuthorizeURL = "https://openrouter.ai/auth"
	pkceSessionTTL    = 15 * time.Minute
	maxPKCESessions   = 32
	pkceFilePrefix    = ".oauth-openrouter-"
	pkceFileSuffix    = ".oauth"
)

func oauthKeysURL() string { return selectedBase(upstreamBase) + "/auth/keys" }

// pkceHTTPDoer executes the key-exchange POST through host transport policy
// (proxy, logging). Both the plugin's own client and the host-injected
// request client satisfy it.
type pkceHTTPDoer interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type pkceSession struct {
	verifier    string
	approvalURL string
	authDir     string
	claimed     bool
	expiresAt   time.Time
}

// pkceLogin serves one provider's OAuth login flow. It implements the
// StartLogin/PollLogin half (wired by shadowing authProvider's embedded kit
// methods). It keeps no management routes: the panel dialog drives the
// approval and the host OAuth callback stores the code.
type pkceLogin struct {
	parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	post  pkceHTTPDoer

	mu       sync.Mutex
	sessions map[string]*pkceSession
}

func newPKCELogin(parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error), post pkceHTTPDoer) (*pkceLogin, error) {
	if parse == nil || post == nil {
		return nil, &pluginabi.Error{Code: "invalid_login", Message: "OAuth login requires the ParseAuth pipeline and an HTTP client", HTTPStatus: 500}
	}
	return &pkceLogin{parse: parse, post: post, sessions: make(map[string]*pkceSession)}, nil
}

// pkceVerifier mints a 43-character unguessable verifier (RFC 7636 minimum).
func pkceVerifier() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func pkceState() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (l *pkceLogin) evictLocked(now time.Time) {
	for state, s := range l.sessions {
		if !now.Before(s.expiresAt) {
			delete(l.sessions, state)
			removeOAuthFile(s.authDir, state)
		}
	}
	for len(l.sessions) >= maxPKCESessions {
		oldest := ""
		for state, s := range l.sessions {
			if oldest == "" || s.expiresAt.Before(l.sessions[oldest].expiresAt) {
				oldest = state
			}
		}
		if oldest == "" {
			return
		}
		removeOAuthFile(l.sessions[oldest].authDir, oldest)
		delete(l.sessions, oldest)
	}
}

// StartLogin mints a pending OAuth session and returns the OpenRouter approval
// URL: the panel opens it in its standard OAuth dialog. The callback URL sent
// to OpenRouter is the host's own loopback base URL tagged with the minted
// state, so the host's OAuth callback accepts the redirect and writes the
// code file PollLogin reads. A remote operator copies the loopback callback
// URL from their address bar into the panel's callback box instead.
func (l *pkceLogin) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "api_key_only", Message: "API-key only: create an auth JSON file", HTTPStatus: 400}
	}
	u, err := url.Parse(strings.TrimSpace(req.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.ContainsAny(u.Host, " \t\r\n'\"") {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "host login base URL is missing", HTTPStatus: 500}
	}
	verifier, err := pkceVerifier()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "cannot mint login state", HTTPStatus: 500}
	}
	state, err := pkceState()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "cannot mint login state", HTTPStatus: 500}
	}
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	callback := u.String()
	authURL := oauthAuthorizeURL + "?callback_url=" + url.QueryEscape(callback) +
		"&code_challenge=" + url.QueryEscape(pkceChallenge(verifier)) +
		"&code_challenge_method=S256"
	authDir := strings.TrimSpace(req.Host.AuthDir)
	sweepStaleOAuthFiles(authDir, time.Now())
	now := time.Now()
	l.mu.Lock()
	l.evictLocked(now)
	l.sessions[state] = &pkceSession{verifier: verifier,
		approvalURL: authURL,
		authDir:     authDir, expiresAt: now.Add(pkceSessionTTL)}
	l.mu.Unlock()
	return pluginapi.AuthLoginStartResponse{Provider: provider, URL: authURL, State: state, ExpiresAt: now.Add(pkceSessionTTL)}, nil
}

func (l *pkceLogin) lookup(state string) *pkceSession {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[state]
	if !ok || !now.Before(s.expiresAt) {
		if ok {
			removeOAuthFile(s.authDir, state)
		}
		delete(l.sessions, state)
		return nil
	}
	return s
}

// validOAuthState gates file use: states are hex minted by StartLogin and
// echoed back by the host, but a state carrying a separator or ".." must
// never reach the filesystem.
func validOAuthState(state string) bool {
	if state == "" || len(state) > 128 {
		return false
	}
	if strings.Contains(state, "/") || strings.Contains(state, "\\") || strings.Contains(state, "..") {
		return false
	}
	for i := 0; i < len(state); i++ {
		c := state[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// oauthFile is the pending code file for a session. The name carries the
// random state, not the key, and the host ignores non-.json auth material.
// The host's OAuth callback writes this same file ({code, state, error}) when
// the operator submits the pasted callback URL through the panel.
func oauthFile(authDir, state string) string {
	name := pkceFilePrefix + state + pkceFileSuffix
	if authDir == "" {
		return name
	}
	return filepath.Join(authDir, name)
}

// removeOAuthFile deletes a session's pending code file, if any. It refuses
// unsafe states and ignores errors: the file is single-use hygiene, and a
// leftover is swept on the next StartLogin.
func removeOAuthFile(authDir, state string) {
	if !validOAuthState(state) {
		return
	}
	_ = os.Remove(oauthFile(authDir, state))
}

// sweepStaleOAuthFiles removes code files left by sessions that died with the
// process (or otherwise outlived their TTL), so abandoned logins cannot
// accumulate files the session map no longer knows.
func sweepStaleOAuthFiles(authDir string, now time.Time) {
	if strings.TrimSpace(authDir) == "" {
		return
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, pkceFilePrefix) || !strings.HasSuffix(name, pkceFileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < pkceSessionTTL {
			continue
		}
		_ = os.Remove(filepath.Join(authDir, name))
	}
}

// PollLogin exchanges a submitted code for an API key exactly once. Until the
// code arrives it reports pending with the approval URL; after a completed or
// expired session it reports an error telling the operator to start over. A
// host-recorded approval error (the operator denied access, or pasted a URL
// without a code) is reported as an error without echoing the callback.
func (l *pkceLogin) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginPollResponse{}, &pluginabi.Error{Code: "api_key_only", Message: "API-key only: create an auth JSON file", HTTPStatus: 400}
	}
	s := l.lookup(req.State)
	if s == nil {
		// The session is gone (expired, consumed, or never existed). Drop
		// any orphan code file so abandoned logins leave nothing behind.
		removeOAuthFile(strings.TrimSpace(req.Host.AuthDir), req.State)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired or not found; start a new login"}, nil
	}
	// The host publishes the file atomically, so a present file is always
	// complete: pending polls before the approval must not claim the session.
	raw, err := os.ReadFile(oauthFile(s.authDir, req.State))
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending,
			Message: "waiting for approval: open " + s.approvalURL + " in a browser, approve, then paste the redirected localhost URL into the management UI callback box; this login completes automatically"}, nil
	}
	l.mu.Lock()
	current, ok := l.sessions[req.State]
	if !ok || current != s {
		l.mu.Unlock()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired or not found; start a new login"}, nil
	}
	if current.claimed {
		l.mu.Unlock()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "login was already collected by another poll; finishing"}, nil
	}
	current.claimed = true
	l.mu.Unlock()
	// A claimed session that cannot produce credentials is dead: drop it so
	// later polls report Error instead of Pending forever on a login that
	// can never complete.
	drop := func() {
		l.mu.Lock()
		if current, ok := l.sessions[req.State]; ok && current == s {
			delete(l.sessions, req.State)
		}
		l.mu.Unlock()
	}
	// The code is single-use: remove the file before exchanging so a retry
	// or a second poller cannot replay it.
	_ = os.Remove(oauthFile(s.authDir, req.State))
	var stored struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &stored) != nil {
		drop()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "stored approval is invalid; start a new login"}, nil
	}
	if strings.TrimSpace(stored.Error) != "" {
		drop()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "OpenRouter reported an approval error; start a new login"}, nil
	}
	if strings.TrimSpace(stored.Code) == "" {
		drop()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "stored approval is invalid; start a new login"}, nil
	}
	key, err := l.exchange(ctx, req.HTTPClient, strings.TrimSpace(stored.Code), s.verifier)
	if err != nil {
		drop()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: err.Error()}, nil
	}
	authDir := s.authDir
	l.mu.Lock()
	if current, ok := l.sessions[req.State]; !ok || current != s {
		l.mu.Unlock()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired or not found; start a new login"}, nil
	}
	delete(l.sessions, req.State)
	l.mu.Unlock()
	candidate, err := json.Marshal(map[string]any{"type": provider, "api_key": key})
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "cannot encode auth storage; start a new login"}, nil
	}
	digest := sha256.Sum256([]byte(key))
	fileName := provider + "-" + hex.EncodeToString(digest[:6]) + ".json"
	path := fileName
	if authDir != "" {
		path = filepath.Join(authDir, fileName)
	}
	out, err := l.parse(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: fileName, Path: path, RawJSON: candidate})
	if err != nil {
		mapped := kit.MapError(err)
		message := mapped.Message
		message = strings.ReplaceAll(message, key, "[redacted]")
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}
	if !out.Handled {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "provider did not accept the login; start a new login"}, nil
	}
	auth := out.Auth
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = fileName
	}
	if strings.TrimSpace(auth.Provider) == "" {
		auth.Provider = provider
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "login complete", Auth: auth, Auths: out.Auths}, nil
}

// exchange trades a one-time code for an API key. Failures are generic: the
// code, verifier, and any returned key never appear in errors.
func (l *pkceLogin) exchange(ctx context.Context, client pkceHTTPDoer, code, verifier string) (string, error) {
	if client == nil {
		client = l.post
	}
	body, err := json.Marshal(map[string]any{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	if err != nil {
		return "", &pluginabi.Error{Code: "login_unavailable", Message: "cannot start the key exchange; start a new login", HTTPStatus: 500}
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: oauthKeysURL(),
		Headers: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}, Body: body})
	if err != nil {
		return "", &pluginabi.Error{Code: "login_unavailable", Message: "key exchange failed; start a new login", HTTPStatus: 502}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", &pluginabi.Error{Code: "login_unavailable", Message: "key exchange was rejected; start a new login", HTTPStatus: 502}
	}
	var got struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(resp.Body, &got) != nil || !validKey(got.Key) {
		return "", &pluginabi.Error{Code: "login_unavailable", Message: "key exchange returned an unusable key; start a new login", HTTPStatus: 502}
	}
	return strings.TrimSpace(got.Key), nil
}
