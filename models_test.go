package main

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newTestCatalog(clock *fakeClock) catalog {
	g := newGuard(newKeyState(nil))
	cat := newCatalog(g)
	cat.now = clock.now
	return cat
}

func modelRequest(host pluginapi.HostHTTPClient) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{AuthID: "auth-1", AuthProvider: provider, StorageJSON: storage(""), HTTPClient: host}
}

func TestModelsForAuthFiltersAndPrefixes(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: fixture(t, "models.json")}
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
		if !strings.HasPrefix(m.ID, modelPrefix) || m.OwnedBy != provider || m.Object != "model" {
			t.Fatalf("unprefixed or unowned model %+v", m)
		}
	}
	want := []string{
		"openrouter/anthropic/claude-sonnet-4.5",
		"openrouter/meta-llama/llama-3.3-70b-instruct:free",
		"openrouter/openrouter/free",
		"openrouter/~anthropic/claude-sonnet-latest",
		"openrouter/mistralai/sunset-model",
		"openrouter/Qwen/Qwen3-Coder-Next",
		"openrouter/deepseek/deepseek-r2",
	}
	if !reflect.DeepEqual(ids, want) || out.Provider != provider {
		t.Fatalf("ids %v", ids)
	}
	req := host.calls()[0]
	if req.Method != http.MethodGet || req.URL != "https://openrouter.ai/api/v1/models" || req.Headers.Get("Authorization") != "Bearer "+exampleKey || req.Headers.Get("Accept") != "application/json" {
		t.Fatalf("%s %s %v", req.Method, req.URL, req.Headers)
	}
	claude := out.Models[0]
	wantClaude := pluginapi.ModelInfo{ID: "openrouter/anthropic/claude-sonnet-4.5", Object: "model", OwnedBy: provider, Name: "openrouter/anthropic/claude-sonnet-4.5", Created: 1759161676, DisplayName: "Anthropic: Claude Sonnet 4.5 via OpenRouter", Description: "A normal paid chat model.", ContextLength: 1000000, MaxCompletionTokens: 64000, OutputTokenLimit: 64000, SupportedParameters: []string{"max_tokens", "temperature", "tools"}, SupportedInputModalities: []string{"text", "image", "file"}, SupportedOutputModalities: []string{"text"}}
	if !reflect.DeepEqual(claude, wantClaude) {
		t.Fatalf("mapping\n got %+v\nwant %+v", claude, wantClaude)
	}
	if alias := out.Models[3]; alias.DisplayName != "~anthropic/claude-sonnet-latest via OpenRouter" {
		t.Fatalf("alias display name %q", alias.DisplayName)
	}
	r2 := out.Models[6]
	if r2.Thinking == nil || !reflect.DeepEqual(r2.Thinking.Levels, []string{"high", "medium", "low"}) || r2.Thinking.ZeroAllowed {
		t.Fatalf("thinking %+v", r2.Thinking)
	}
	if out.Models[1].Thinking != nil {
		t.Fatal("thinking invented for a non-reasoning model")
	}
}

// Every identifier the plugin publishes carries the fixed namespace: the host
// lists Name as the Gemini model name, and its auth-prefix rewrite only
// rewrites a Name equal to the ID.
func TestEveryModelIdentifierIsNamespaced(t *testing.T) {
	out, err := newTestCatalog(newClock(now0)).ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: fixture(t, "models.json")}))
	if err != nil || len(out.Models) == 0 {
		t.Fatal(err)
	}
	var alias, free bool
	for _, m := range out.Models {
		if !strings.HasPrefix(m.ID, modelPrefix) || m.Name != m.ID {
			t.Errorf("un-namespaced identifier: ID %q Name %q", m.ID, m.Name)
		}
		alias = alias || strings.HasPrefix(m.ID, modelPrefix+"~")
		free = free || strings.HasSuffix(m.ID, ":free")
	}
	if !alias || !free {
		t.Fatal("fixture must cover ~ aliases and :free variants")
	}
}

func TestModelsForAuthCache(t *testing.T) {
	clock := newClock(now0)
	cat := newTestCatalog(clock)
	host := &fakeHost{body: fixture(t, "models.json")}
	for i := 0; i < 3; i++ {
		if _, err := cat.ModelsForAuth(context.Background(), modelRequest(host)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(host.calls()); n != 1 {
		t.Fatalf("cached catalog re-fetched %d times", n)
	}
	other := modelRequest(host)
	other.StorageJSON = []byte(`{"api_key":"rotated-key"}`)
	if _, err := cat.ModelsForAuth(context.Background(), other); err != nil || len(host.calls()) != 2 {
		t.Fatal("a rotated credential must not reuse the cache")
	}
	clock.advance(catalogTTL + time.Second)
	if _, err := cat.ModelsForAuth(context.Background(), other); err != nil || len(host.calls()) != 3 {
		t.Fatal("expired cache not refreshed")
	}
}

func TestModelsForAuthErrors(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	for _, tc := range []struct {
		host *fakeHost
		code string
	}{
		{&fakeHost{body: fixture(t, "models_not_json.txt")}, "invalid_models"},
		{&fakeHost{body: []byte(`{"object":"list"}`)}, "invalid_models"},
		{&fakeHost{status: 401, body: fixture(t, "models_401.json")}, "invalid_credentials"},
		{&fakeHost{body: []byte(`{"data":[],"pad":"` + strings.Repeat("x", maxModelsBody) + `"}`)}, "invalid_models"},
	} {
		_, err := cat.ModelsForAuth(context.Background(), modelRequest(tc.host))
		if e := rpcErr(t, err); e.Code != tc.code || strings.Contains(e.Message, exampleKey) {
			t.Fatalf("%+v", e)
		}
	}
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(nil)); rpcErr(t, err).Code != "host_unavailable" {
		t.Fatal("missing host client accepted")
	}
	if out, err := cat.ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: []byte(`{"data":[]}`)})); err != nil || out.Models == nil || len(out.Models) != 0 {
		t.Fatalf("empty catalog %+v %v", out, err)
	}
}

func TestStaticRegistrationIsEmpty(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	reg, err := cat.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || reg.Provider != provider || len(reg.Models) != 0 {
		t.Fatalf("%+v %v", reg, err)
	}
	static, err := cat.StaticModels(context.Background(), pluginapi.StaticModelRequest{Host: pluginapi.HostConfigSummary{ProxyURL: "direct"}})
	if err != nil || static.Provider != provider || len(static.Models) != 0 {
		t.Fatalf("%+v %v", static, err)
	}
	if p := cat.guard.hostProxy.Load(); p == nil || *p != "direct" {
		t.Fatal("static model request did not record the host proxy")
	}
}

func TestModelIDPattern(t *testing.T) {
	for id, want := range map[string]bool{"a/b": true, "~a/b-latest": true, "a/b:free": true, "A.b_c/D-e.f~g": true, "a": false, "a/b/c": false, "/b": false, "a/": false, "a b/c": false, "~~a/b": false, "a/b?x": false, "-a/b": false} {
		if got := modelIDPattern.MatchString(id); got != want {
			t.Errorf("%q: %v", id, got)
		}
	}
	long := "v/" + strings.Repeat("m", maxModelID)
	if keepModel(modelWire{ID: long}, now0) {
		t.Fatal("over-long id kept")
	}
}
