//go:build cgo && (linux || darwin || freebsd)

// Harness layout adapted from cliproxy-provider-synthetic (MIT).
package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	_ "unsafe" // go:linkname, see applyModelPrefixes

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator" // the host's built-in translators, as in cmd/server
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

const (
	provider = "openrouter"
	version  = "0.1.0"
	modelA   = "openrouter/anthropic/claude-sonnet-4.5"
	modelB   = "openrouter/free"
)

// applyModelPrefixes is the host's own auth-prefix expansion
// (sdk/cliproxy/service_models.go). It is unexported, and the service that
// calls it needs a full server, so the test binds the pinned function itself
// rather than re-implementing it.
//
//go:linkname applyModelPrefixes github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy.applyModelPrefixes
func applyModelPrefixes(models []*cliproxy.ModelInfo, prefix string, forceModelPrefix bool) []*cliproxy.ModelInfo

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type models struct{}

func (models) RegisterClient(string, string, []*registry.ModelInfo) {}
func (models) UnregisterClient(string)                              {}
func (models) GetModelProviders(string) []string                    { return nil }

const healthyKey = `{"data":{"limit":25,"limit_remaining":10.5,"limit_reset":"monthly","usage":14.5,"usage_daily":1,"usage_weekly":2,"usage_monthly":14.5,"is_free_tier":false,"is_management_key":false,"free_model_daily_requests":{"used":3,"limit":1000,"remaining":997}}}`

const catalog = `{"data":[
{"id":"anthropic/claude-sonnet-4.5","name":"Anthropic: Claude Sonnet 4.5","context_length":1000000,"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"pricing":{"prompt":"0.000003","completion":"0.000015"}},
{"id":"openrouter/free","name":"Free Models Router","architecture":{"input_modalities":["text"],"output_modalities":["text"]},"pricing":{"prompt":"0","completion":"0"}},
{"id":"openrouter/auto","name":"Auto Router","pricing":{"prompt":"-1","completion":"-1"}},
{"id":"~anthropic/claude-sonnet-latest","name":"","pricing":{"prompt":"0.000003","completion":"0.000015"}}
]}`

// packedTokens are the deltas of packedSSE, in order.
var packedTokens = []string{"OK", "!", " How", " can", " I", " assist", " you", " today", "?"}

var packedSSE = func() string {
	var b strings.Builder
	b.WriteString(": OPENROUTER PROCESSING\n\n")
	for i, tok := range packedTokens {
		delta := map[string]any{"content": tok}
		if i == 0 {
			delta["role"] = "assistant"
		}
		raw, _ := json.Marshal(map[string]any{"id": "gen-example", "object": "chat.completion.chunk", "model": "anthropic/claude-sonnet-4.5", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
		b.WriteString("data: " + string(raw) + "\n\n")
		if i == 3 {
			b.WriteString(": OPENROUTER PROCESSING\n\n")
		}
	}
	b.WriteString(`data: {"id":"gen-example","object":"chat.completion.chunk","model":"anthropic/claude-sonnet-4.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString(`data: {"id":"gen-example","object":"chat.completion.chunk","model":"anthropic/claude-sonnet-4.5","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}()

// sseData returns the data payload of every event in a client SSE body, and
// fails on a frame that is not a well-formed event.
func sseData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case strings.HasPrefix(line, "event: "):
			default:
				t.Fatalf("malformed SSE line %q in\n%s", line, body)
			}
		}
		if len(data) != 1 {
			t.Fatalf("frame without exactly one data line %q", frame)
		}
		if d := data[0]; d != "[DONE]" && !json.Valid([]byte(d)) {
			t.Fatalf("event data is not JSON: %q", d)
		}
		out = append(out, data[0])
	}
	return out
}

// upstream is a loopback stand-in for openrouter.ai. Behavior is keyed by the
// API key so one plugin instance serves every scenario.
type upstream struct {
	t       *testing.T
	resetAt time.Time
	mu      sync.Mutex
	chats   map[string]int
	spent   atomic.Bool
	models  atomic.Int32
	gate    chan struct{}
}

func (u *upstream) chatCount(key string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chats[key]
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/key":
		switch {
		case key == "nolimit-key":
			io.WriteString(w, `{"data":{"limit":null,"limit_remaining":null,"limit_reset":null,"usage":1,"is_management_key":false}}`)
		case key == "limited-key" && u.spent.Load():
			io.WriteString(w, `{"data":{"limit":2,"limit_remaining":-0.01,"limit_reset":"daily","usage":2.01,"is_management_key":false,"budgets":[{"scope":"key_limit","limit_usd":2,"spend_usd":2.01,"remaining_usd":-0.01,"reset_interval":"daily","resets_at":"`+u.resetAt.Format(time.RFC3339)+`"}]}}`)
		default:
			io.WriteString(w, healthyKey)
		}
	case "/credits":
		// The regular key reads the account balance (observed live).
		io.WriteString(w, `{"data":{"total_credits":245,"total_usage":237.537304708}}`)
	case "/models":
		u.models.Add(1)
		io.WriteString(w, catalog)
	case "/chat/completions":
		u.mu.Lock()
		u.chats[key]++
		u.mu.Unlock()
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			u.t.Error("invalid inference JSON")
			w.WriteHeader(400)
			return
		}
		model, _ := body["model"].(string)
		if strings.HasPrefix(model, "openrouter/anthropic") || model == "free" || model == "openrouter/openrouter/free" {
			u.t.Errorf("prefix not stripped exactly once: %q", model)
		}
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "nolimit-key":
			u.t.Error("a no-limit key reached inference")
			w.WriteHeader(500)
			return
		case "limited-key", "spent-key", "lagging-key":
			u.spent.Store(true)
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"code":403,"message":"Key limit exceeded (daily limit).","metadata":{"limit_source":"openrouter_key_limit","scope":"key_limit","resets_at":"`+u.resetAt.Format(time.RFC3339)+`"}}}`)
			return
		case "inflight-key":
			w.WriteHeader(402)
			io.WriteString(w, `{"error":{"code":402,"message":"in-flight budget","metadata":{"reason":"in_flight_budget_exhausted","limit_source":"openrouter_in_flight_budget"}}}`)
			return
		case "policy-key":
			if body["fixture"] == "weight" {
				w.WriteHeader(402)
				io.WriteString(w, `{"error":{"code":402,"message":"too large","metadata":{"reason":"weight_exceeds_budget","limit_source":"openrouter_credits"}}}`)
			} else {
				w.WriteHeader(403)
				io.WriteString(w, `{"error":{"code":403,"message":"Input was flagged by moderation","metadata":{"reasons":["example"]}}}`)
			}
			return
		}
		if body["stream"] == true && key == "packed-key" {
			// Every event and a comment in one write, as OpenRouter can send them.
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, packedSSE)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, ": OPENROUTER PROCESSING\n\ndata: {\"id\":\"gen-example\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
			if body["fixture_gate"] == true {
				select {
				case <-u.gate:
				case <-r.Context().Done():
					return
				}
			}
			io.WriteString(w, "data: {\"id\":\"gen-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"id":"gen-example","object":"chat.completion","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	default:
		u.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(404)
	}
}

func statusOf(err error) int {
	if e, ok := err.(interface{ StatusCode() int }); ok {
		return e.StatusCode()
	}
	return 0
}

// The actual v8.0.49 loader performs dlopen, C-ABI registration, RPC dispatch,
// persistence and executor adapters. Only the upstream is an httptest mock.
func TestRealHostLoader(t *testing.T) {
	up := &upstream{t: t, chats: map[string]int{}, gate: make(chan struct{}), resetAt: time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)}
	server := httptest.NewServer(up)
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0o755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib := filepath.Join(arch, provider+ext)
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", lib, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", err, out)
	}
	if out, err := exec.Command("nm", "-D", lib).CombinedOutput(); err != nil || !strings.Contains(string(out), " T cliproxy_plugin_init") {
		t.Fatalf("ABI entry point: %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_base: "+server.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{provider: {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx = logging.WithResponseHeadersHolder(ctx)
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded(provider) || !host.PluginRegistered(provider) || !host.HasAuthProvider(provider) || !host.HasQuotaProvider(provider) {
		t.Fatalf("registration %+v", host.RegisteredPlugins())
	}
	if info := host.RegisteredPlugins(); len(info) != 1 || !info[0].SupportsQuota || info[0].Metadata.Version != version {
		t.Fatalf("metadata %+v", info)
	}
	parse := func(file, raw string) *coreauth.Auth {
		t.Helper()
		a, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: file, RawJSON: []byte(raw)})
		if err != nil || !handled || a == nil {
			t.Fatalf("auth %s: %v %v", file, handled, err)
		}
		return a
	}
	auth := parse("example.json", `{"type":"openrouter","api_key":"example-key","label":"Example"}`)
	if _, _, err = host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "duplicate.json", RawJSON: []byte(`{"type":"openrouter","api_key":"example-key"}`)}); err == nil {
		t.Fatal("duplicate key accepted by real host parser")
	}
	if _, _, err = host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "typo.json", RawJSON: []byte(`{"type":"openrouter","api_key":"typo-key","min_remaining":5}`)}); err == nil {
		t.Fatal("unknown field accepted by real host parser")
	}
	file := filepath.Join(cfg.AuthDir, "example.json")
	if err = auth.Storage.SaveTokenToFile(file); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(saved, &fields) != nil || fields["type"] != provider || fields["label"] != "Example" {
		t.Fatal("host auth storage merge failed")
	}
	// A host-persisted file must parse again (restart path).
	if _, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "example.json", RawJSON: saved}); err != nil || !handled {
		t.Fatalf("persisted auth no longer parses: %v", err)
	}
	quota, handled, err := host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: provider, StorageJSON: saved})
	if err != nil || !handled || len(quota.Groups) != 3 || quota.Groups[0].Buckets[0].RemainingFraction != .42 || quota.Groups[0].Buckets[0].ResetTime == "" || quota.Groups[2].DisplayName != "Account credits" {
		t.Fatalf("quota %+v %v %v", quota, handled, err)
	}

	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	// (10) The empty static catalog is accepted: no static models, and the
	// executor and per-auth discovery are still registered.
	if static := host.ModelsForProvider(provider); len(static) != 0 {
		t.Fatalf("static models %+v", static)
	}
	ex, ok := m[provider]
	if !ok || !host.HasAuthModelProvider(provider) {
		t.Fatal("executor or per-auth model provider not registered with an empty static catalog")
	}
	// (1) Only openrouter/ IDs are registered by the plugin.
	found := host.ModelsForAuth(ctx, auth)
	var ids []string
	for _, model := range found.Models {
		ids = append(ids, model.ID)
		if !strings.HasPrefix(model.ID, "openrouter/") {
			t.Fatalf("unprefixed discovery ID %q", model.ID)
		}
	}
	if !found.Handled || strings.Join(ids, ",") != "openrouter/anthropic/claude-sonnet-4.5,openrouter/free,openrouter/~anthropic/claude-sonnet-latest" {
		t.Fatalf("discovery %v", ids)
	}
	// (9) Re-discovery is host-driven: a second call within 10 minutes is
	// served from the plugin cache. The pinned host re-runs ModelsForAuth on
	// auth (re)registration and plugin reload only (docs/SOURCES.md).
	_ = host.ModelsForAuth(ctx, auth)
	if up.models.Load() != 1 {
		t.Fatalf("catalog fetched %d times", up.models.Load())
	}
	t.Run("auth prefix adds prefix/openrouter/ IDs, never bare vendor IDs", func(t *testing.T) {
		acct := parse("acct.json", `{"type":"openrouter","api_key":"acct-key","prefix":"acct"}`)
		if acct.Prefix != "acct" {
			t.Fatalf("prefix %q", acct.Prefix)
		}
		discovered := host.ModelsForAuth(ctx, acct)
		published := map[string]bool{}
		for _, model := range applyModelPrefixes(discovered.Models, acct.Prefix, false) {
			published[model.ID] = true
			// Name is the Gemini listing's model name: namespaced as well.
			if model.Name != model.ID {
				t.Fatalf("published name %q for ID %q", model.Name, model.ID)
			}
		}
		if !published["acct/openrouter/anthropic/claude-sonnet-4.5"] || !published["openrouter/anthropic/claude-sonnet-4.5"] || published["anthropic/claude-sonnet-4.5"] || published["acct/anthropic/claude-sonnet-4.5"] {
			t.Fatalf("published IDs %v", published)
		}
		for id := range published {
			if !strings.HasPrefix(id, "openrouter/") && !strings.HasPrefix(id, "acct/openrouter/") {
				t.Fatalf("unexpected published ID %q", id)
			}
		}
	})

	opts := coreexec.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI}
	req := coreexec.Request{Model: modelA, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"hello"}]}`)}
	resp, err := ex.Execute(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(resp.Payload), `"content":"hello"`) {
		t.Fatalf("execute %s %v", resp.Payload, err)
	}
	// (7) Streaming is relayed unbuffered, one event per chunk.
	t.Run("streaming is unbuffered", func(t *testing.T) {
		streamReq := coreexec.Request{Model: modelB, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + modelB + `","stream":true,"fixture_gate":true,"messages":[{"role":"user","content":"hello"}]}`)}
		streamOpts := opts
		streamOpts.Stream = true
		stream, err := ex.ExecuteStream(ctx, auth, streamReq, streamOpts)
		if err != nil {
			t.Fatal(err)
		}
		var chunks []string
		select {
		case c := <-stream.Chunks:
			if c.Err != nil || len(c.Payload) == 0 {
				t.Fatalf("first chunk %+v", c)
			}
			chunks = append(chunks, string(c.Payload))
			close(up.gate)
		case <-time.After(20 * time.Second):
			t.Fatal("stream buffered until completion")
		}
		for c := range stream.Chunks {
			if c.Err != nil {
				t.Fatal(c.Err)
			}
			chunks = append(chunks, string(c.Payload))
		}
		// One event per chunk; the comment and [DONE] are dropped. With no
		// inbound route recorded, events keep their "data:" framing.
		if len(chunks) != 2 || !strings.HasPrefix(chunks[0], `data: {"id":"gen-example"`) || !strings.Contains(chunks[1], `"content":"lo"`) || strings.Contains(strings.Join(chunks, ""), "[DONE]") {
			t.Fatalf("stream %q", chunks)
		}
	})

	register := func(t *testing.T, mgr *coreauth.Manager, a *coreauth.Auth, id string, priority string, ids ...string) *coreauth.Auth {
		t.Helper()
		a = a.Clone()
		a.ID = id
		a.Status = coreauth.StatusActive
		a.Attributes = map[string]string{"type": "api_key", "priority": priority}
		if _, err := mgr.Register(ctx, a); err != nil {
			t.Fatal(err)
		}
		infos := []*registry.ModelInfo{}
		for _, model := range ids {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, infos)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		return a
	}
	call := func(model string, extra string) coreexec.Request {
		return coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `"` + extra + `,"messages":[{"role":"user","content":"hello"}]}`)}
	}

	t.Run("client SSE through the real OpenAI and Claude handlers", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		register(t, mgr, parse("packed.json", `{"type":"openrouter","api_key":"packed-key"}`), "packed-auth", "0", modelA)
		base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, mgr)
		router := gin.New()
		router.POST("/v1/chat/completions", openaihandlers.NewOpenAIAPIHandler(base).ChatCompletions)
		router.POST("/v1/messages", claudehandlers.NewClaudeCodeAPIHandler(base).ClaudeMessages)
		post := func(t *testing.T, path, body string) string {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d %s", path, rec.Code, rec.Body.String())
			}
			out := rec.Body.String()
			if strings.Contains(out, "data: data:") || strings.Contains(out, "OPENROUTER PROCESSING") {
				t.Fatalf("%s: raw upstream framing reached the client:\n%s", path, out)
			}
			return out
		}
		want := strings.Join(packedTokens, "")

		t.Run("OpenAI chat", func(t *testing.T) {
			out := post(t, "/v1/chat/completions", `{"model":"`+modelA+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if strings.Count(out, "[DONE]") != 1 || !strings.HasSuffix(out, "data: [DONE]\n\n") {
				t.Fatalf("[DONE] framing:\n%s", out)
			}
			var text strings.Builder
			for _, d := range sseData(t, out) {
				var ev struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if d != "[DONE]" && json.Unmarshal([]byte(d), &ev) == nil && len(ev.Choices) > 0 {
					text.WriteString(ev.Choices[0].Delta.Content)
				}
			}
			if text.String() != want {
				t.Fatalf("tokens %q, want %q\n%s", text.String(), want, out)
			}
		})

		t.Run("Claude messages", func(t *testing.T) {
			out := post(t, "/v1/messages", `{"model":"`+modelA+`","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			var text strings.Builder
			stops := 0
			for _, d := range sseData(t, out) {
				var ev struct {
					Type  string `json:"type"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(d), &ev) != nil {
					t.Fatalf("event %q", d)
				}
				if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
					text.WriteString(ev.Delta.Text)
				}
				if ev.Type == "message_stop" {
					stops++
				}
			}
			if text.String() != want || stops != 1 {
				t.Fatalf("tokens %q (want %q), message_stop %d\n%s", text.String(), want, stops, out)
			}
		})
	})

	t.Run("no-limit key is refused before any inference call", func(t *testing.T) {
		_, err := ex.Execute(ctx, parse("nolimit.json", `{"type":"openrouter","api_key":"nolimit-key"}`), call(modelA, ""), opts)
		if statusOf(err) != 403 || !strings.Contains(err.Error(), "key_limit_required") || up.chatCount("nolimit-key") != 0 {
			t.Fatalf("%v calls=%d", err, up.chatCount("nolimit-key"))
		}
	})

	t.Run("key-limit exhaustion cools every model until the reset", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		limited := register(t, mgr, parse("limited.json", `{"type":"openrouter","api_key":"limited-key"}`), "limited-auth", "0", modelA, modelB)
		// (2) A 403 key limit with resets_at reaches the client as 429 and
		// cools the model until resets_at, not for the fixed 30 minutes.
		_, err := mgr.Execute(ctx, []string{provider}, call(modelA, ""), opts)
		if statusOf(err) != 429 || up.chatCount("limited-key") != 1 {
			t.Fatalf("exhaustion %v calls=%d", err, up.chatCount("limited-key"))
		}
		near := func(state *coreauth.ModelState) bool {
			return state != nil && state.Quota.Exceeded && !state.NextRetryAfter.Before(up.resetAt.Add(-3*time.Second)) && !state.NextRetryAfter.After(up.resetAt.Add(3*time.Second))
		}
		got, _ := mgr.GetByID(limited.ID)
		if !near(got.ModelStates[modelA]) {
			t.Fatalf("cooldown is not the 2 h reset: %+v", got.ModelStates[modelA])
		}
		if _, err = mgr.Execute(ctx, []string{provider}, call(modelA, ""), opts); err == nil || up.chatCount("limited-key") != 1 {
			t.Fatalf("dispatched during cooldown: %v calls=%d", err, up.chatCount("limited-key"))
		}
		// (3) A different model on the same auth is refused by the guard with
		// zero upstream calls, and cools until the same reset.
		_, err = mgr.Execute(ctx, []string{provider}, call(modelB, ""), opts)
		if statusOf(err) != 429 || up.chatCount("limited-key") != 1 {
			t.Fatalf("guard did not refuse the other model: %v calls=%d", err, up.chatCount("limited-key"))
		}
		got, _ = mgr.GetByID(limited.ID)
		if !near(got.ModelStates[modelB]) {
			t.Fatalf("guard refusal lost the reset: %+v", got.ModelStates[modelB])
		}
	})

	t.Run("lagging key counters: other models are still refused locally until the reset", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		lagging := register(t, mgr, parse("lagging.json", `{"type":"openrouter","api_key":"lagging-key"}`), "lagging-auth", "0", modelA, modelB)
		if _, err := mgr.Execute(ctx, []string{provider}, call(modelA, ""), opts); statusOf(err) != 429 || up.chatCount("lagging-key") != 1 {
			t.Fatalf("exhaustion %v calls=%d", err, up.chatCount("lagging-key"))
		}
		// GET /key still reports headroom, yet modelB is refused with no
		// upstream call and cools until the same reset.
		_, err := mgr.Execute(ctx, []string{provider}, call(modelB, ""), opts)
		if statusOf(err) != 429 || up.chatCount("lagging-key") != 1 {
			t.Fatalf("latch did not refuse the other model: %v calls=%d", err, up.chatCount("lagging-key"))
		}
		got, _ := mgr.GetByID(lagging.ID)
		state := got.ModelStates[modelB]
		if state == nil || !state.Quota.Exceeded || state.NextRetryAfter.Before(up.resetAt.Add(-3*time.Second)) || state.NextRetryAfter.After(up.resetAt.Add(3*time.Second)) {
			t.Fatalf("latch refusal lost the reset: %+v", state)
		}
	})

	t.Run("quota exhaustion fails over to a second openrouter auth", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		mgr.SetRetryConfig(0, 0, 2)
		mgr.RegisterExecutor(ex)
		first := register(t, mgr, parse("spent.json", `{"type":"openrouter","api_key":"spent-key"}`), "spent-auth", "10", modelA)
		second := register(t, mgr, parse("second.json", `{"type":"openrouter","api_key":"second-key"}`), "second-auth", "0", modelA)
		if _, err := mgr.Execute(ctx, []string{provider}, call(modelA, ""), opts); err != nil {
			t.Fatalf("failover %v", err)
		}
		failed, _ := mgr.GetByID(first.ID)
		healthy, _ := mgr.GetByID(second.ID)
		if state := failed.ModelStates[modelA]; state == nil || !state.Quota.Exceeded || state.NextRetryAfter.Before(time.Now().Add(time.Hour)) {
			t.Fatalf("failed auth state %+v", state)
		}
		if healthy.Unavailable || healthy.Quota.Exceeded || up.chatCount("second-key") != 1 {
			t.Fatalf("healthy sibling impaired %+v", healthy)
		}
	})

	t.Run("in-flight 402 cools briefly at the host floor", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		a := register(t, mgr, parse("inflight.json", `{"type":"openrouter","api_key":"inflight-key"}`), "inflight-auth", "0", modelA)
		before := time.Now()
		_, err := mgr.Execute(ctx, []string{provider}, call(modelA, ""), opts)
		if statusOf(err) != 429 {
			t.Fatalf("%v", err)
		}
		got, _ := mgr.GetByID(a.ID)
		state := got.ModelStates[modelA]
		if state == nil || state.NextRetryAfter.Before(before.Add(9*time.Second)) || state.NextRetryAfter.After(time.Now().Add(11*time.Second)) {
			t.Fatalf("in-flight cooldown %+v", state)
		}
	})

	t.Run("request-scoped 400s do not cool the model", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		a := register(t, mgr, parse("policy.json", `{"type":"openrouter","api_key":"policy-key"}`), "policy-auth", "0", modelA)
		for _, fx := range []string{"weight", "moderation"} {
			_, err := mgr.Execute(ctx, []string{provider}, call(modelA, `,"fixture":"`+fx+`"`), opts)
			if statusOf(err) != 400 {
				t.Fatalf("%s: %v", fx, err)
			}
			got, _ := mgr.GetByID(a.ID)
			if state := got.ModelStates[modelA]; state != nil && state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("%s cooled the model until %v", fx, state.NextRetryAfter)
			}
		}
		if up.chatCount("policy-key") != 2 {
			t.Fatalf("calls %d", up.chatCount("policy-key"))
		}
	})
}

func TestRegistryUsesNativeStoreSchema(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := pluginstore.ParseRegistry(raw)
	if err != nil || reg.SchemaVersion != 1 || len(reg.Plugins) != 1 || reg.Plugins[0].ID != provider || reg.Plugins[0].Version != version || pluginstore.PluginInstallType(reg.Plugins[0]) != "github-release" {
		t.Fatalf("registry %+v %v", reg, err)
	}
}
