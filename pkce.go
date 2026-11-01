package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
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
// with an AuthProvider. The flow, with no host change:
//
//  1. StartLogin mints a PKCE verifier/challenge pair plus a random state and
//     returns the OpenRouter approval URL. The operator opens it in a
//     browser, signs in, and approves; OpenRouter redirects to the
//     host-loopback callback URL with ?code=...
//  2. The host runs headless, so a remote operator's browser cannot reach
//     that loopback address. The kit-served OAuth page instead shows a
//     copy-paste shell block: the operator pastes the callback URL from
//     their address bar into their terminal, and it travels over stdin in
//     an authenticated management POST. The secrets (management key in the
//     header file, callback URL in the body) never land in shell history,
//     process arguments, URLs, or logs.
//  3. The submit handler extracts the one-time code and stores it in
//     .oauth-openrouter-<state>.oauth in the auth directory.
//  4. PollLogin reads that file, exchanges the code for an API key with
//     POST /api/v1/auth/keys (code + verifier, S256), deletes the file,
//     and runs the provider's full ParseAuth pipeline, including the
//     duplicate-key guard, so the login faces the same checks as an
//     uploaded auth file.
//
// The verifier never leaves the plugin process except in the exchange POST
// body; the code is single-use and the file is removed once read.
const (
	oauthAuthorizeURL = "https://openrouter.ai/auth"
	pkceSessionTTL    = 15 * time.Minute
	maxPKCESessions   = 32
	pkcePageRoute     = "oauth"
	pkceSubmitPrefix  = "plugin-oauth/"
	pkceFilePrefix    = ".oauth-openrouter-"
	pkceFileSuffix    = ".oauth"
	maxPKCESubmitBody = 16 << 10
)

func oauthKeysURL() string { return selectedBase(upstreamBase) + "/auth/keys" }

// pkceHTTPDoer executes the key-exchange POST through host transport policy
// (proxy, logging). Both the plugin's own client and the host-injected
// request client satisfy it.
type pkceHTTPDoer interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type pkceSession struct {
	verifier  string
	callback  string
	authDir   string
	expiresAt time.Time
}

// pkceLogin serves one provider's OAuth login flow. It implements the
// StartLogin/PollLogin half (wired by shadowing authProvider's embedded
// kit methods) and the ManagementAPI/ManagementHandler halves for
// Capabilities.ManagementAPI.
type pkceLogin struct {
	parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	post  pkceHTTPDoer

	mu           sync.Mutex
	sessions     map[string]*pkceSession
	mgmtBase     string
	resourceBase string
}

func newPKCELogin(parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error), post pkceHTTPDoer) (*pkceLogin, error) {
	if parse == nil || post == nil {
		return nil, &pluginabi.Error{Code: "invalid_login", Message: "OAuth login requires the ParseAuth pipeline and an HTTP client", HTTPStatus: 500}
	}
	return &pkceLogin{parse: parse, post: post, sessions: make(map[string]*pkceSession), mgmtBase: "/v0/management"}, nil
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
		delete(l.sessions, oldest)
	}
}

// StartLogin mints a pending OAuth session and returns the approval URL. The
// callback is the host's own base URL: a browser on the host machine
// completes the redirect directly, while a remote operator pastes the
// callback URL into the OAuth page's shell block instead.
func (l *pkceLogin) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "api_key_only", Message: "API-key only: create an auth JSON file", HTTPStatus: 400}
	}
	callback := strings.TrimSpace(req.BaseURL)
	u, err := url.Parse(callback)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
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
	authURL := oauthAuthorizeURL + "?callback_url=" + url.QueryEscape(callback) +
		"&code_challenge=" + url.QueryEscape(pkceChallenge(verifier)) +
		"&code_challenge_method=S256"
	now := time.Now()
	l.mu.Lock()
	l.evictLocked(now)
	l.sessions[state] = &pkceSession{verifier: verifier, callback: callback,
		authDir: strings.TrimSpace(req.Host.AuthDir), expiresAt: now.Add(pkceSessionTTL)}
	l.mu.Unlock()
	return pluginapi.AuthLoginStartResponse{Provider: provider, URL: authURL, State: state, ExpiresAt: now.Add(pkceSessionTTL)}, nil
}

func (l *pkceLogin) lookup(state string) *pkceSession {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[state]
	if !ok || !now.Before(s.expiresAt) {
		delete(l.sessions, state)
		return nil
	}
	return s
}

// oauthFile is the pending code file for a session. The name carries the
// random state, not the key, and the host ignores non-.json auth material.
func oauthFile(authDir, state string) string {
	name := pkceFilePrefix + state + pkceFileSuffix
	if authDir == "" {
		return name
	}
	return filepath.Join(authDir, name)
}

// PollLogin exchanges a submitted code for an API key exactly once. Until the
// code arrives it reports pending with the approval URL; after a completed or
// expired session it reports an error telling the operator to start over.
func (l *pkceLogin) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginPollResponse{}, &pluginabi.Error{Code: "api_key_only", Message: "API-key only: create an auth JSON file", HTTPStatus: 400}
	}
	s := l.lookup(req.State)
	if s == nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired or not found; start a new login"}, nil
	}
	raw, err := os.ReadFile(oauthFile(s.authDir, req.State))
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending,
			Message: "waiting for approval: open the login URL in a browser, approve, then paste the callback URL into the OAuth page"}, nil
	}
	// The code is single-use: remove the file before exchanging so a retry
	// or a second poller cannot replay it.
	_ = os.Remove(oauthFile(s.authDir, req.State))
	var stored struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &stored) != nil || strings.TrimSpace(stored.Code) == "" {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "stored approval is invalid; start a new login"}, nil
	}
	key, err := l.exchange(ctx, req.HTTPClient, strings.TrimSpace(stored.Code), s.verifier)
	if err != nil {
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

// RegisterManagement declares the OAuth paste-box page (resource route) and
// the callback-URL submit endpoint (management route).
func (l *pkceLogin) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	if l == nil {
		return pluginapi.ManagementRegistrationResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "login is not configured", HTTPStatus: 500}
	}
	base := strings.TrimSpace(req.BasePath)
	if base == "" {
		base = "/v0/management"
	}
	l.mu.Lock()
	l.mgmtBase = strings.TrimRight(base, "/")
	l.resourceBase = strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/")
	l.mu.Unlock()
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method:      http.MethodPost,
			Path:        pkceSubmitPrefix + provider,
			Description: "Submit a pasted OpenRouter OAuth callback URL for a pending login started from the management UI.",
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        pkcePageRoute,
			Description: "Paste an OpenRouter OAuth callback URL to complete a pending login started from the management UI.",
		}},
	}, nil
}

// HandleManagement dispatches the declared routes by exact method and full
// path. Request headers are intentionally ignored: the host forwards
// management credentials with the request, and the login flow must never
// read, log, or echo them.
func (l *pkceLogin) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if l == nil {
		return pkceJSON(http.StatusInternalServerError, "login_unavailable", "login is not configured"), nil
	}
	l.mu.Lock()
	mgmtBase, resourceBase := l.mgmtBase, l.resourceBase
	l.mu.Unlock()
	if mgmtBase == "" {
		mgmtBase = "/v0/management"
	}
	switch {
	case req.Method == http.MethodGet && req.Path == resourceBase+"/"+pkcePageRoute && resourceBase != "":
		return l.servePage(req), nil
	case req.Path == mgmtBase+"/"+pkceSubmitPrefix+provider:
		if req.Method != http.MethodPost {
			return pkceJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST"), nil
		}
		return l.serveSubmit(req), nil
	default:
		return pkceJSON(http.StatusNotFound, "unknown_route", "no OAuth route"), nil
	}
}

func pkceJSON(status int, code, message string) pluginapi.ManagementResponse {
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message}})
	if err != nil {
		raw = []byte(`{"error":{"code":"encoding_error","message":"response encoding failed"}}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{StatusCode: status,
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: raw}
}

// servePage renders the paste-box page for a pending session. The page holds
// the session state and a copy-paste shell block only; the callback URL is
// typed into the operator's terminal, never into this page. The block is
// brace-grouped and carries both secrets over stdin for the same reasons as
// the kit's API-key login page.
func (l *pkceLogin) servePage(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	l.mu.Lock()
	mgmtBase := l.mgmtBase
	l.mu.Unlock()
	if mgmtBase == "" {
		mgmtBase = "/v0/management"
	}
	submitURL := mgmtBase + "/" + pkceSubmitPrefix + provider
	state := req.Query.Get("state")
	s := l.lookup(state)
	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	b.WriteString("<title>" + html.EscapeString(provider) + " OAuth login</title></head><body>")
	b.WriteString("<h1>" + html.EscapeString(provider) + " OAuth login</h1>")
	if s == nil {
		b.WriteString("<p>This login session expired or does not exist. In the management UI, click <b>Start " +
			html.EscapeString(provider) + " Login</b> again to get a fresh login page.</p>")
	} else {
		b.WriteString("<p>Open the approval URL from the login card in a browser and approve. " +
			"OpenRouter redirects to a host-loopback address your browser cannot reach: " +
			"copy the full callback URL from your address bar and paste it into the block below. " +
			"Set CLIPROXY_BASE to the host's public base URL (for example https://host:port). " +
			"On a remote host always use https:// so the management key and the callback URL " +
			"are encrypted in transit; plain http:// is only acceptable on a loopback address you trust. " +
			"The URL travels in the POST body only: it never appears in the pasted text, " +
			"shell history, process arguments, or logs.</p>")
		var cmd strings.Builder
		cmd.WriteString("{\n")
		cmd.WriteString("printf 'Management key: '; read -rs CLIPROXY_MANAGEMENT_KEY; echo\n")
		cmd.WriteString("printf 'Callback URL: '; read -r PLUGIN_CALLBACK_URL; echo\n")
		cmd.WriteString("CLEAN_MGMT=${CLIPROXY_MANAGEMENT_KEY//$'\\n'/}; CLEAN_MGMT=${CLEAN_MGMT//$'\\r'/}\n")
		cmd.WriteString("ESC_CB=${PLUGIN_CALLBACK_URL//\\\\/\\\\\\\\}; ESC_CB=${ESC_CB//\\\"/\\\\\\\"}; " +
			"ESC_CB=${ESC_CB//$'\\n'/\\\\n}; ESC_CB=${ESC_CB//$'\\r'/\\\\r}; ESC_CB=${ESC_CB//$'\\t'/\\\\t}\n")
		cmd.WriteString(`curl -X POST "$CLIPROXY_BASE` + submitURL + `" ` +
			"\\\n  -H @<(printf '%s' \"Authorization: Bearer $CLEAN_MGMT\") \\\n  -H 'Content-Type: application/json' \\\n" +
			"  --data-binary @- <<EOF\n" + `{"state":"` + state + `","callback_url":"$ESC_CB"}` + "\nEOF\n")
		cmd.WriteString("unset CLIPROXY_MANAGEMENT_KEY PLUGIN_CALLBACK_URL CLEAN_MGMT ESC_CB\n}\n")
		b.WriteString("<pre>" + html.EscapeString(cmd.String()) + "</pre>")
		b.WriteString("<p>Keep polling this login in the management UI; " +
			"it completes a few seconds after the POST succeeds. This page expires " +
			html.EscapeString(s.expiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")) + ".</p>")
		b.WriteString("<p>Your management key authenticates you to the host; this page never asks for it and the plugin ignores request headers.</p>")
	}
	b.WriteString("</body></html>")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK,
		Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: []byte(b.String())}
}

// serveSubmit extracts the one-time code from a pasted callback URL and
// stores it for PollLogin. Errors never echo the URL or the code.
func (l *pkceLogin) serveSubmit(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > maxPKCESubmitBody {
		return pkceJSON(http.StatusBadRequest, "invalid_request", "request body too large")
	}
	var fields map[string]any
	dec := json.NewDecoder(strings.NewReader(string(req.Body)))
	if dec.Decode(&fields) != nil || fields == nil {
		return pkceJSON(http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
	}
	state, _ := fields["state"].(string)
	s := l.lookup(state)
	if s == nil {
		return pkceJSON(http.StatusBadRequest, "login_expired", "login session expired or not found; start a new login")
	}
	rawURL, _ := fields["callback_url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return pkceJSON(http.StatusBadRequest, "invalid_auth", "a callback URL is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return pkceJSON(http.StatusBadRequest, "invalid_auth", "callback URL is not a URL")
	}
	code := strings.TrimSpace(u.Query().Get("code"))
	if code == "" {
		if u.Query().Get("error") != "" {
			return pkceJSON(http.StatusBadRequest, "invalid_auth", "OpenRouter reported an approval error; start a new login")
		}
		return pkceJSON(http.StatusBadRequest, "invalid_auth", "callback URL has no approval code; approve first, then paste the redirected URL")
	}
	if s.authDir == "" {
		return pkceJSON(http.StatusBadRequest, "login_unavailable", "host auth directory is missing; start a new login")
	}
	stored, err := json.Marshal(map[string]any{"code": code})
	if err != nil {
		return pkceJSON(http.StatusBadRequest, "invalid_auth", "cannot store the approval; start a new login")
	}
	if err := os.WriteFile(oauthFile(s.authDir, state), stored, 0600); err != nil {
		return pkceJSON(http.StatusBadGateway, "login_unavailable", "cannot store the approval; start a new login")
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)}
}
