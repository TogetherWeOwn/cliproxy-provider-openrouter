package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newTestCatalog(clock *fakeClock) *catalog {
	g := newGuard(newKeyState(nil))
	cat := newCatalog(g)
	cat.now = clock.now
	return cat
}

func modelRequest(host pluginapi.HostHTTPClient) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{AuthID: "auth-1", AuthProvider: provider, StorageJSON: storage(""), HTTPClient: host}
}

func smallCatalog(ids ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"` + id + `","name":"` + id + `","pricing":{"prompt":"0","completion":"0"}}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
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
		"openrouter/free",
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

// The executor strips exactly one leading namespace before the upstream
// request, so every emitted id must round-trip through upstreamID.
func TestEmittedIDsRoundTripThroughExecutorMapping(t *testing.T) {
	out, err := newTestCatalog(newClock(now0)).ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: fixture(t, "models.json")}))
	if err != nil {
		t.Fatal(err)
	}
	wantUpstream := map[string]string{
		"openrouter/anthropic/claude-sonnet-4.5":            "anthropic/claude-sonnet-4.5",
		"openrouter/meta-llama/llama-3.3-70b-instruct:free": "meta-llama/llama-3.3-70b-instruct:free",
		"openrouter/free": "openrouter/free",
		"openrouter/~anthropic/claude-sonnet-latest": "~anthropic/claude-sonnet-latest",
		"openrouter/mistralai/sunset-model":          "mistralai/sunset-model",
		"openrouter/Qwen/Qwen3-Coder-Next":           "Qwen/Qwen3-Coder-Next",
		"openrouter/deepseek/deepseek-r2":            "deepseek/deepseek-r2",
	}
	if len(out.Models) != len(wantUpstream) {
		t.Fatalf("models %d, want %d", len(out.Models), len(wantUpstream))
	}
	for _, m := range out.Models {
		up, ok := upstreamID(m.ID)
		if !ok || up != wantUpstream[m.ID] {
			t.Fatalf("upstreamID(%q) = %q, %v; want %q, true", m.ID, up, ok, wantUpstream[m.ID])
		}
	}
}

// Kit contributor dedup for previously unknown ids: with a "-contributor"
// fixture sibling the contributor entry wins and the base entry is dropped;
// a model with no sibling keeps its base id. No per-model override is
// needed — the pairing is discovered from the source list itself.
func TestModelCatalogContributorDedupForUnknownIDs(t *testing.T) {
	out, err := newTestCatalog(newClock(now0)).ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: smallCatalog("acme/new-model", "acme/new-model-contributor", "acme/solo")}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	want := []string{"openrouter/acme/new-model-contributor", "openrouter/acme/solo"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	// Executor mapping: stripping one namespace reaches the contributor id.
	if got := strings.TrimPrefix(ids[0], modelPrefix); got != "acme/new-model-contributor" {
		t.Fatalf("executor upstream id %q, want contributor variant", got)
	}
	if err := kit.CheckModelNamespace(provider, out.Models); err != nil {
		t.Fatalf("runtime namespace check: %v", err)
	}
}

// Timeline A: every ModelsForAuth invocation fetches, so a newly upstream
// model converges on the next host event even inside the freshness TTL.
func TestModelsForAuthTimelineAFreshConverges(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: smallCatalog("acme/a")}
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(host)); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.body = smallCatalog("acme/a", "acme/b")
	host.mu.Unlock()
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if want := []string{"openrouter/acme/a", "openrouter/acme/b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	if n := len(host.calls()); n != 2 {
		t.Fatalf("fetches %d, want every invocation to fetch (2)", n)
	}
}

// Timeline B (documented, never a guarantee): a transient failure serves
// durable last-good and publishes nothing new; no further invocation is
// guaranteed — the kit starts no background work, so the fetch count stays
// stable until the next host event.
func TestModelsForAuthTimelineBFailureKeepsLastGood(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: smallCatalog("acme/a")}
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(host)); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.status = http.StatusBadGateway
	host.body = []byte(`{"error":"boom"}`)
	host.mu.Unlock()
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "openrouter/acme/a" {
		t.Fatalf("timeline B served %+v, want last-good", out.Models)
	}
	before := len(host.calls())
	time.Sleep(50 * time.Millisecond)
	if n := len(host.calls()); n != before {
		t.Fatalf("fetches %d, want %d: no background fetch without a host callback", n, before)
	}
}

// A failed fetch never erases last-good, however far past the freshness TTL
// the failure happens: last-good is durable, not TTL-bound.
func TestModelsForAuthLastGoodSurvivesFailure(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: smallCatalog("acme/a")}
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(host)); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.status = http.StatusUnauthorized
	host.body = fixture(t, "models_401.json")
	host.mu.Unlock()
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "openrouter/acme/a" {
		t.Fatalf("served %+v, want durable last-good after 401", out.Models)
	}
	// An oversized refresh is likewise a failed fetch, not a replacement.
	host.mu.Lock()
	host.status = 0
	host.body = []byte(`{"data":[],"pad":"` + strings.Repeat("x", maxModelsBody) + `"}`)
	host.mu.Unlock()
	out, err = cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "openrouter/acme/a" {
		t.Fatalf("served %+v, want durable last-good after oversize", out.Models)
	}
}

// A rotated credential must never receive the old generation's list, and a
// fetch failure with no last-good and no static surfaces the error.
func TestModelsForAuthCredentialGenerationIsolation(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: smallCatalog("acme/a")}
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(host)); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.status = http.StatusBadGateway
	host.body = []byte(`{"error":"boom"}`)
	host.mu.Unlock()
	rotated := modelRequest(host)
	rotated.StorageJSON = []byte(`{"api_key":"rotated-key"}`)
	_, err := cat.ModelsForAuth(context.Background(), rotated)
	if e := rpcErr(t, err); e.Code != "upstream_unavailable" {
		t.Fatalf("rotated fetch err %+v, want upstream_unavailable (no stale last-good)", e)
	}
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "openrouter/acme/a" {
		t.Fatalf("original generation served %+v, want last-good", out.Models)
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
	// An empty upstream result is a failed fetch in the kit pipeline: with
	// no last-good and no static, the fetch error surfaces.
	if _, err := cat.ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: []byte(`{"data":[]}`)})); rpcErr(t, err).Code != "models_fetch_failed" {
		t.Fatalf("empty upstream err %v, want models_fetch_failed", err)
	}
}

// The Fetch's parse cap is plugin-local (16 MiB). A catalog larger than
// the kit's 1 MiB OpenAIModelsFetcher cap must still be served: that cap
// bounds only the kit fetcher, never this custom Fetch.
func TestModelsForAuthLargeCatalogServed(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	body := []byte(`{"data":[{"id":"acme/big","name":"acme/big","pricing":{"prompt":"0","completion":"0"}}],"pad":"` + strings.Repeat("x", 2<<20) + `"}`)
	if len(body) <= kit.MaxModelsBody || len(body) > maxModelsBody {
		t.Fatalf("fixture body %d bytes outside (1 MiB, 16 MiB]", len(body))
	}
	out, err := cat.ModelsForAuth(context.Background(), modelRequest(&fakeHost{body: body}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "openrouter/acme/big" {
		t.Fatalf("served %+v, want the model from a catalog above the kit cap", out.Models)
	}
}

// Mutating a returned list must not corrupt later responses.
func TestModelsForAuthCopyIsolation(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := &fakeHost{body: smallCatalog("acme/a")}
	first, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	first.Models[0].DisplayName = "mutated"
	first.Models[0].ID = "mutated"
	second, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Models) != 1 || second.Models[0].ID != "openrouter/acme/a" || second.Models[0].DisplayName != "acme/a via OpenRouter" {
		t.Fatalf("second served %+v, want unmutated copy", second.Models)
	}
}

// blockingHost parks each Do call until the request context is done, so
// cancellation and deadline tests are deterministic rather than racy.
type blockingHost struct {
	mu        sync.Mutex
	entered   chan struct{}
	sawDone   bool
	callCount int
}

func newBlockingHost() *blockingHost {
	return &blockingHost{entered: make(chan struct{}, 1)}
}

func (b *blockingHost) Do(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	b.mu.Lock()
	b.callCount++
	b.mu.Unlock()
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	b.mu.Lock()
	b.sawDone = true
	b.mu.Unlock()
	return pluginapi.HTTPResponse{}, ctx.Err()
}

func (b *blockingHost) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming not supported")
}

func (b *blockingHost) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callCount
}

// Parent cancellation propagates into the host call derived by the kit's
// fetch deadline; with no fallback available the context error surfaces.
func TestModelsForAuthParentCancellation(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := newBlockingHost()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cat.ModelsForAuth(ctx, modelRequest(host))
		done <- err
	}()
	select {
	case <-host.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the blocking host call")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled err %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call did not return")
	}
	host.mu.Lock()
	saw := host.sawDone
	host.mu.Unlock()
	if !saw {
		t.Fatal("host client never saw parent cancellation")
	}
}

// An earlier parent deadline beats the kit's derived fetch timeout.
func TestModelsForAuthParentDeadline(t *testing.T) {
	cat := newTestCatalog(newClock(now0))
	host := newBlockingHost()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cat.ModelsForAuth(ctx, modelRequest(host))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("blocked fetch returned after %s, want bounded by the parent deadline", elapsed)
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
