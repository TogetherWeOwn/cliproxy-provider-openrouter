package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const keyLimitNoReset = `{"error":{"code":402,"message":"Key limit exceeded","metadata":{"limit_source":"openrouter_key_limit"}}}`

// execute sends one non-streaming request for model through the executor.
func execute(h *harness, host *fakeHost, model string) error {
	_, err := h.exec.Execute(context.Background(), execRequest(host, `{"model":"openrouter/`+model+`"}`))
	return err
}

// After OpenRouter reports a spent key limit, a re-read GET /key can still
// show headroom because its counters lag. The latch refuses every model of
// the auth locally, at no upstream cost, until the reset.
func TestSpendLatchSurvivesLaggingCounters(t *testing.T) {
	h := liveHarness(t)
	spent := &fakeHost{status: 402, body: []byte(keyLimitNoReset)}
	e := rpcErr(t, execute(h, spent, "a/b"))
	monday := retrySeconds(utc("2026-11-02T00:00:00Z"), liveNow)
	if e.Code != "quota_exhausted" || e.RetryAfterSeconds != monday {
		t.Fatalf("%+v", e)
	}
	ok := &fakeHost{body: []byte(`{"choices":[{"index":0}]}`)}
	for _, model := range []string{"c/d", "vendor/model:free", "a/b"} {
		e = rpcErr(t, execute(h, ok, model))
		if e.Code != "quota_exhausted" || e.RetryAfterSeconds != monday || !strings.Contains(e.Message, "spent limit") {
			t.Fatalf("%s: %+v", model, e)
		}
	}
	if n := len(ok.calls()); n != 0 {
		t.Fatalf("latched auth dispatched %d upstream calls", n)
	}
	if calls := h.srv.keyCalls.Load(); calls != 2 {
		t.Fatalf("the reading must be re-read once after the error: %d", calls)
	}
	// Still latched after later re-reads that show the same caps.
	h.clock.advance(2 * time.Minute)
	if e = rpcErr(t, execute(h, ok, "c/d")); e.Code != "quota_exhausted" || h.srv.keyCalls.Load() != 3 {
		t.Fatalf("%+v calls=%d", e, h.srv.keyCalls.Load())
	}
	// At the reset the latch ends.
	h.clock.advance(time.Duration(monday) * time.Second)
	if err := execute(h, ok, "c/d"); err != nil || len(ok.calls()) != 1 {
		t.Fatalf("latch outlived the reset: %v", err)
	}
}

// Raising the key limit lifts the latch at the next reading.
func TestSpendLatchLiftsWhenTheOwnerRaisesTheLimit(t *testing.T) {
	h := liveHarness(t)
	_ = execute(h, &fakeHost{status: 402, body: []byte(keyLimitNoReset)}, "a/b")
	ok := &fakeHost{body: []byte(`{"choices":[{"index":0}]}`)}
	if rpcErr(t, execute(h, ok, "a/b")).Code != "quota_exhausted" {
		t.Fatal("not latched")
	}
	raised := strings.Replace(string(fixture(t, "live_key_inference.json")), `"limit": 5,`, `"limit": 10,`, 1)
	h.srv.set([]byte(raised), 200)
	h.clock.advance(keyTTL + time.Second)
	if err := execute(h, ok, "a/b"); err != nil || len(ok.calls()) != 1 {
		t.Fatalf("raised limit still latched: %v", err)
	}
}

// Account credits exhausted: latched for the hourly re-check unless /credits
// shows a top-up. A failed /credits read is not a top-up.
func TestCreditLatchLiftsOnTopUp(t *testing.T) {
	h := liveHarness(t)
	_ = execute(h, &fakeHost{status: 402, body: fixture(t, "err_402_credits.json")}, "a/b")
	ok := &fakeHost{body: []byte(`{"choices":[{"index":0}]}`)}
	if e := rpcErr(t, execute(h, ok, "a/b")); e.Code != "quota_exhausted" || e.RetryAfterSeconds != recheckSeconds {
		t.Fatalf("%+v", e)
	}
	h.srv.mu.Lock()
	h.srv.creditsCode, h.srv.credits = 503, []byte(`{"error":{"code":503}}`)
	h.srv.mu.Unlock()
	h.clock.advance(keyTTL + time.Second)
	if rpcErr(t, execute(h, ok, "a/b")).Code != "quota_exhausted" {
		t.Fatal("a failed /credits read lifted the latch")
	}
	h.srv.mu.Lock()
	h.srv.creditsCode, h.srv.credits = 200, []byte(`{"data":{"total_credits":300,"total_usage":237.537304708}}`)
	h.srv.mu.Unlock()
	h.clock.advance(keyTTL + time.Second)
	if err := execute(h, ok, "a/b"); err != nil {
		t.Fatalf("top-up still latched: %v", err)
	}
}

// The daily free-model quota latches only the auth's :free models, until
// 00:00 UTC, even though the lagging reading still shows requests left.
func TestFreeLatchBlocksOnlyFreeModels(t *testing.T) {
	h := liveHarness(t)
	e := rpcErr(t, execute(h, &fakeHost{status: 429, body: fixture(t, "err_429_free.json")}, "meta-llama/llama-3.3-70b-instruct:free"))
	midnight := retrySeconds(utc("2026-11-02T00:00:00Z"), liveNow)
	if e.Code != "quota_exhausted" || e.RetryAfterSeconds != midnight {
		t.Fatalf("%+v", e)
	}
	ok := &fakeHost{body: []byte(`{"choices":[{"index":0}]}`)}
	if e = rpcErr(t, execute(h, ok, "qwen/qwen3-coder:free")); e.Code != "quota_exhausted" || !strings.Contains(e.Message, "free-model") || e.RetryAfterSeconds != midnight {
		t.Fatalf("%+v", e)
	}
	if err := execute(h, ok, "anthropic/claude-sonnet-4.5"); err != nil {
		t.Fatalf("paid model refused by the free latch: %v", err)
	}
	h.clock.advance(time.Duration(midnight) * time.Second)
	if err := execute(h, ok, "qwen/qwen3-coder:free"); err != nil {
		t.Fatalf("free latch outlived the day: %v", err)
	}
}

// A rotated key starts with no latch.
func TestLatchIsPerCredential(t *testing.T) {
	h := liveHarness(t)
	_ = execute(h, &fakeHost{status: 402, body: []byte(keyLimitNoReset)}, "a/b")
	err := h.guard.admit(context.Background(), "auth-1", credentials{apiKey: "rotated-key"}, storage(""), nil, "a/b")
	if err != nil {
		t.Fatalf("a rotated key inherited the latch: %v", err)
	}
}
