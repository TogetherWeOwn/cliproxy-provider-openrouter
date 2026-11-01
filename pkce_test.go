package main

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errFakeNoStream = errors.New("fake client does not stream")

const (
	pkceResourceBase = "/v0/resource/plugins/openrouter-id"
	pkceSubmitPath   = "/v0/management/plugin-oauth/openrouter"
	pkceBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
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
	if _, err := pkce.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: pkceResourceBase}); err != nil {
		t.Fatal(err)
	}
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
}

// TestPKCELoginRoundTrip drives approval URL, paste-box submit, exchange,
// and poll through the provider's own ParseAuth pipeline.
func TestPKCELoginRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := &fakePKCEClient{status: http.StatusOK, body: `{"key":"sk-or-roundtrip-key"}`}
	p, pkce := pkceProvider(t, fake)
	authDir := t.TempDir()

	start := startPKCE(t, p, authDir)
	if !strings.HasPrefix(start.URL, oauthAuthorizeURL+"?") ||
		!strings.Contains(start.URL, "code_challenge=") ||
		!strings.Contains(start.URL, "code_challenge_method=S256") ||
		!strings.Contains(start.URL, "callback_url=") {
		t.Fatalf("unexpected approval URL %q", start.URL)
	}
	if start.State == "" {
		t.Fatal("empty login state")
	}

	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}

	page, err := pkce.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET",
		Path: pkceResourceBase + "/oauth", Query: map[string][]string{"state": {start.State}}})
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("OAuth page %+v %v", page, err)
	}
	if body := html.UnescapeString(string(page.Body)); !strings.Contains(body, "read -r PLUGIN_CALLBACK_URL") {
		t.Fatal("page missing the paste-box block")
	}

	submit, err := pkce.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: pkceSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","callback_url":"http://127.0.0.1:9999/cb?code=authcode123&state=ignored"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
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

// TestPKCESubmitRejectsBadCallbacks covers missing/denied codes, unknown
// states, and malformed bodies without echoing the callback URL.
func TestPKCESubmitRejectsBadCallbacks(t *testing.T) {
	ctx := context.Background()
	p, _ := pkceProvider(t, &fakePKCEClient{})
	authDir := t.TempDir()
	start := startPKCE(t, p, authDir)
	for _, tc := range []struct {
		name, state, body, want string
	}{
		{"unknown state", "nope", `{"state":"nope","callback_url":"http://127.0.0.1:9/cb?code=x"}`, "login_expired"},
		{"not json", start.State, `{`, "invalid_request"},
		{"missing url", start.State, `{"state":"` + start.State + `"}`, "a callback URL is required"},
		{"not a url", start.State, `{"state":"` + start.State + `","callback_url":":::"}`, "not a URL"},
		{"no code", start.State, `{"state":"` + start.State + `","callback_url":"http://127.0.0.1:9/cb?state=x"}`, "no approval code"},
		{"denied", start.State, `{"state":"` + start.State + `","callback_url":"http://127.0.0.1:9/cb?error=access_denied"}`, "approval error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := p.oauth.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
				Path: pkceSubmitPath, Body: []byte(tc.body)})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 400 || !strings.Contains(string(resp.Body), tc.want) {
				t.Fatalf("got %+v, want %q", resp, tc.want)
			}
			if strings.Contains(string(resp.Body), "authcode") || strings.Contains(string(resp.Body), "127.0.0.1:9/cb") {
				t.Fatalf("rejection echoed the callback: %s", resp.Body)
			}
		})
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
	submit, err := p.oauth.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: pkceSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","callback_url":"http://127.0.0.1:9/cb?code=spentcode"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
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
