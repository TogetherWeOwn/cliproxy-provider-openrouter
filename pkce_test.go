package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errFakeNoStream = errors.New("fake client does not stream")

const (
	pkceBaseURL = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

// fakePKCEClient answers the key-exchange POST. It records the request body
// so tests can assert the code and verifier actually sent.
type fakePKCEClient struct {
	status int
	body   string
	calls  int
	gotURL string
	got    []byte
}

func (f *fakePKCEClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.calls++
	f.gotURL = req.URL
	f.got = req.Body
	return pluginapi.HTTPResponse{StatusCode: f.status, Body: []byte(f.body)}, nil
}

func (f *fakePKCEClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errFakeNoStream
}

func pkceProvider(t *testing.T, client pkceHTTPDoer) (*authProvider, *pkceLogin) {
	t.Helper()
	p := newAuthProvider(newGuard(nil))
	pkce, err := newPKCELogin(p.ParseAuth, client)
	if err != nil {
		t.Fatal(err)
	}
	p.oauth = pkce
	return p, pkce
}

func startPKCE(t *testing.T, p *authProvider, authDir string) pluginapi.AuthLoginStartResponse {
	t.Helper()
	start, err := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "openrouter",
		BaseURL: pkceBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: authDir}})
	if err != nil {
		t.Fatal(err)
	}
	return start
}

// TestPKCEChallengeVector pins the S256 challenge to RFC 7636 Appendix B.
func TestPKCEChallengeVector(t *testing.T) {
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("wrong challenge %q", got)
	}
	if v, err := pkceVerifier(); err != nil || len(v) != 43 {
		t.Fatalf("verifier len %d err %v", len(v), err)
	}
}

func TestPKCELoginRequiresConfig(t *testing.T) {
	if _, err := newPKCELogin(nil, &fakePKCEClient{}); err == nil {
		t.Fatal("nil parse accepted")
	}
	p := newAuthProvider(newGuard(nil))
	if _, err := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("unwired login started")
	}
	if _, err := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "openrouter"}); err == nil {
		t.Fatal("login started without a host base URL")
	}
	// No management routes are needed: the panel dialog drives the approval
	// and the host OAuth callback stores the code.
	unregistered, err := newPKCELogin(p.ParseAuth, &fakePKCEClient{})
	if err != nil {
		t.Fatal(err)
	}
	p.oauth = unregistered
	if _, err := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "openrouter",
		BaseURL: pkceBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: t.TempDir()}}); err != nil {
		t.Fatalf("login needs no registered routes: %v", err)
	}
}

// storeHostCode writes the code file the way the host OAuth callback does
// when the operator submits the pasted loopback URL through the panel.
func storeHostCode(t *testing.T, authDir, state, code string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"code": code, "state": state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oauthFile(authDir, state), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestPKCELoginRoundTrip drives approval URL, host callback file, exchange,
// and poll through the provider's own ParseAuth pipeline.
func TestPKCELoginRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := &fakePKCEClient{status: http.StatusOK, body: `{"key":"sk-or-roundtrip-key"}`}
	p, _ := pkceProvider(t, fake)
	authDir := t.TempDir()

	start := startPKCE(t, p, authDir)
	// The login card opens the OpenRouter approval URL directly, the official
	// pattern: the callback is the host loopback base tagged with the state,
	// and the link carries the S256 challenge.
	if start.State == "" {
		t.Fatal("empty login state")
	}
	approval, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Scheme != "https" || approval.Host != "openrouter.ai" || approval.Path != "/auth" {
		t.Fatalf("login URL %q is not the OpenRouter approval URL", start.URL)
	}
	aq := approval.Query()
	if aq.Get("code_challenge") == "" || aq.Get("code_challenge_method") != "S256" {
		t.Fatalf("approval link lost PKCE: %s", approval.RawQuery)
	}
	callback, err := url.Parse(aq.Get("callback_url"))
	if err != nil || callback.Query().Get("state") != start.State {
		t.Fatalf("callback lost the login state: %q", aq.Get("callback_url"))
	}

	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}
	if !strings.Contains(poll.Message, "callback box") {
		t.Fatalf("pending does not point at the panel callback box: %q", poll.Message)
	}

	storeHostCode(t, authDir, start.State, "authcode123")
	stored, err := os.ReadFile(filepath.Join(authDir, pkceFilePrefix+start.State+pkceFileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	var code struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(stored, &code) != nil || code.Code != "authcode123" {
		t.Fatalf("code file wrong: %s", stored)
	}
	if info, err := os.Stat(filepath.Join(authDir, pkceFilePrefix+start.State+pkceFileSuffix)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("code file not 0600: %v", info)
	}

	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v %v", done, err)
	}
	if fake.calls != 1 || fake.gotURL != oauthKeysURL() {
		t.Fatalf("exchange not called once at %q: %+v", oauthKeysURL(), fake)
	}
	var sent map[string]any
	if json.Unmarshal(fake.got, &sent) != nil || sent["code"] != "authcode123" || sent["code_verifier"] == "" || sent["code_challenge_method"] != "S256" {
		t.Fatalf("exchange body wrong: %s", fake.got)
	}
	var store map[string]any
	if json.Unmarshal(done.Auth.StorageJSON, &store) != nil || store["api_key"] != "sk-or-roundtrip-key" || done.Auth.Provider != "openrouter" {
		t.Fatalf("key lost through login: %s", done.Auth.StorageJSON)
	}
	if _, err := os.Stat(filepath.Join(authDir, pkceFilePrefix+start.State+pkceFileSuffix)); !os.IsNotExist(err) {
		t.Fatal("single-use code file not removed")
	}
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("consumed login not closed: %+v %v", again, err)
	}
}

// TestPKCEHostErrorFile covers the host-recorded approval error (the operator
// denied access, or pasted a URL without a code): the login closes with an
// error that never echoes the callback, and the file is gone.
func TestPKCEHostErrorFile(t *testing.T) {
	ctx := context.Background()
	p, _ := pkceProvider(t, &fakePKCEClient{})
	authDir := t.TempDir()
	start := startPKCE(t, p, authDir)
	raw, err := json.Marshal(map[string]any{"state": start.State, "error": "access_denied"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oauthFile(authDir, start.State), raw, 0600); err != nil {
		t.Fatal(err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("denied approval not an error: %+v %v", done, err)
	}
	if !strings.Contains(done.Message, "approval error") {
		t.Fatalf("wrong error message: %q", done.Message)
	}
	if strings.Contains(done.Message, "access_denied") {
		t.Fatalf("error echoed the callback: %q", done.Message)
	}
	if _, err := os.Stat(oauthFile(authDir, start.State)); !os.IsNotExist(err) {
		t.Fatal("error file left for replay")
	}
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("denied login not closed: %+v %v", again, err)
	}
}

// TestPKCEInvalidCodeFileCloses covers a claimed-but-useless code file: corrupt
// JSON, or a record with neither code nor error (e.g. the host persisted a
// pasted approval URL instead of the redirect URL). The first poll errors, and
// the dead session is dropped so later polls stay Error instead of regressing
// to Pending forever on a login that can never complete.
func TestPKCEInvalidCodeFileCloses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		raw  func(state string) []byte
	}{
		{"corrupt", func(state string) []byte { return []byte(`{"code":`) }},
		{"codeless", func(state string) []byte { return []byte(`{"state":"` + state + `"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := pkceProvider(t, &fakePKCEClient{})
			authDir := t.TempDir()
			start := startPKCE(t, p, authDir)
			if err := os.WriteFile(oauthFile(authDir, start.State), tc.raw(start.State), 0600); err != nil {
				t.Fatal(err)
			}
			done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
			if err != nil || done.Status != pluginapi.AuthLoginStatusError {
				t.Fatalf("invalid approval not an error: %+v %v", done, err)
			}
			if !strings.Contains(done.Message, "stored approval is invalid") {
				t.Fatalf("wrong error message: %q", done.Message)
			}
			again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
			if err != nil || again.Status != pluginapi.AuthLoginStatusError {
				t.Fatalf("dead login regressed to %+v %v; want Error", again, err)
			}
			if again.Status == pluginapi.AuthLoginStatusPending {
				t.Fatalf("dead login reports Pending: %q", again.Message)
			}
		})
	}
}

// blockingPKCEClient holds the exchange open so a second poller is forced to
// overlap it.
type blockingPKCEClient struct {
	fakePKCEClient
	release chan struct{}
}

func (b *blockingPKCEClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return b.fakePKCEClient.Do(ctx, req)
}

// TestPKCEConcurrentPollSingleExchange proves exactly one poller runs the
// exchange: the loser stays pending and cannot poison the host session with a
// duplicate-exchange error.
func TestPKCEConcurrentPollSingleExchange(t *testing.T) {
	ctx := context.Background()
	gated := &blockingPKCEClient{
		fakePKCEClient: fakePKCEClient{status: http.StatusOK, body: `{"key":"sk-or-concurrent-key"}`},
		release:        make(chan struct{}),
	}
	p, _ := pkceProvider(t, gated)
	authDir := t.TempDir()
	start := startPKCE(t, p, authDir)
	storeHostCode(t, authDir, start.State, "racecode")

	var wg sync.WaitGroup
	got := make([]pluginapi.AuthLoginPollResponse, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = r
		}(i)
	}
	// Let both pollers reach the exchange before releasing it.
	time.Sleep(200 * time.Millisecond)
	close(gated.release)
	wg.Wait()
	var success, pending int
	for _, r := range got {
		switch r.Status {
		case pluginapi.AuthLoginStatusSuccess:
			success++
		case pluginapi.AuthLoginStatusPending:
			pending++
		default:
			t.Fatalf("poller got %+v", r)
		}
	}
	if success != 1 || pending != 1 || gated.calls != 1 {
		t.Fatalf("success=%d pending=%d exchanges=%d", success, pending, gated.calls)
	}
}

// TestPKCEEvictionRemovesCodeFile proves capacity eviction takes the evicted
// session's pending secret file with it.
func TestPKCEEvictionRemovesCodeFile(t *testing.T) {
	p, _ := pkceProvider(t, &fakePKCEClient{})
	authDir := t.TempDir()
	first := startPKCE(t, p, authDir)
	storeHostCode(t, authDir, first.State, "evictme")
	file := filepath.Join(authDir, pkceFilePrefix+first.State+pkceFileSuffix)
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPKCESessions; i++ {
		startPKCE(t, p, authDir)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("evicted session left its code file")
	}
}

// TestPKCEUnknownStateCleansOrphan proves a poll for a dead session removes
// its leftover code file, and that a hostile state never reaches the
// filesystem.
func TestPKCEUnknownStateCleansOrphan(t *testing.T) {
	ctx := context.Background()
	p, _ := pkceProvider(t, &fakePKCEClient{})
	authDir := t.TempDir()
	orphan := filepath.Join(authDir, pkceFilePrefix+"orphanstate1"+pkceFileSuffix)
	if err := os.WriteFile(orphan, []byte(`{"code":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: "orphanstate1",
		Host: pluginapi.HostConfigSummary{AuthDir: authDir}})
	if err != nil || done.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("orphan poll %+v %v", done, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan code file left behind")
	}
	done, err = p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: "../../evil",
		Host: pluginapi.HostConfigSummary{AuthDir: authDir}})
	if err != nil || done.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("hostile poll %+v %v", done, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(authDir, "*evil*")); len(matches) > 0 {
		t.Fatalf("hostile state touched the filesystem: %v", matches)
	}
}

func TestValidOAuthState(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"abc123", true},
		{"A-_.9", true},
		{"", false},
		{"a b", false},
		{"a/b", false},
		{`a\b`, false},
		{"../x", false},
		{strings.Repeat("a", 129), false},
	} {
		if got := validOAuthState(tc.state); got != tc.want {
			t.Errorf("validOAuthState(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestPKCESweepRemovesStaleOnly proves a new login sweeps code files older
// than the session TTL (process-restart leftovers) while keeping fresh ones.
func TestPKCESweepRemovesStaleOnly(t *testing.T) {
	p, _ := pkceProvider(t, &fakePKCEClient{})
	authDir := t.TempDir()
	stale := filepath.Join(authDir, pkceFilePrefix+"stale111"+pkceFileSuffix)
	fresh := filepath.Join(authDir, pkceFilePrefix+"fresh111"+pkceFileSuffix)
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte(`{"code":"x"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * pkceSessionTTL)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	startPKCE(t, p, authDir)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale code file not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh code file swept: %v", err)
	}
}

// TestPKCEExchangeFailure proves a rejected exchange closes the login, leaks
// nothing, and cannot be replayed.
func TestPKCEExchangeFailure(t *testing.T) {
	ctx := context.Background()
	fake := &fakePKCEClient{status: http.StatusBadRequest, body: `{"error":"bad"}`}
	p, _ := pkceProvider(t, fake)
	authDir := t.TempDir()
	start := startPKCE(t, p, authDir)
	storeHostCode(t, authDir, start.State, "spentcode")
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State, HTTPClient: fake})
	if err != nil || done.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("failed exchange not an error: %+v %v", done, err)
	}
	if strings.Contains(done.Message, "spentcode") {
		t.Fatalf("poll error leaked the code: %q", done.Message)
	}
	if _, err := os.Stat(filepath.Join(authDir, pkceFilePrefix+start.State+pkceFileSuffix)); !os.IsNotExist(err) {
		t.Fatal("spent code file left for replay")
	}
}
