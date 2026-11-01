package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func admitWith(h *harness, c credentials, model string) error {
	return h.guard.admit(context.Background(), "auth-1", c, storage(""), nil, model)
}

func secondsUntil(s string) int { return retrySeconds(utc(s), now0) }

func TestGuardRules(t *testing.T) {
	key := credentials{apiKey: exampleKey}
	for _, tc := range []struct {
		name    string
		file    string
		c       credentials
		model   string
		status  int
		code    string
		retry   int
		message string
	}{
		{"G1 management key", "key_management.json", key, "a/b", 401, "invalid_credentials", 0, "management key"},
		{"G1 legacy provisioning key", "key_provisioning_legacy.json", key, "a/b", 401, "invalid_credentials", 0, "management key"},
		{"G2 no key limit", "key_no_limit.json", key, "a/b", 403, "key_limit_required", 0, "set a credit limit"},
		{"G3 negative remaining waits for monthly reset", "key_exhausted_negative.json", key, "a/b", 429, "quota_exhausted", secondsUntil("2026-12-01T00:00:00Z"), "credit limit reached"},
		{"G3 zero daily limit", "key_zero_limit.json", key, "a/b", 429, "quota_exhausted", secondsUntil("2026-11-05T00:00:00Z"), "credit limit reached"},
		{"G3 reserve on weekly key", "key_weekly_limit.json", credentials{apiKey: exampleKey, reserve: 0.01}, "a/b", 429, "quota_exhausted", secondsUntil("2026-11-09T00:00:00Z"), "reserve reached"},
		{"G3 lifetime limit re-checks hourly", "key_lifetime_limit.json", credentials{apiKey: exampleKey, reserve: 60}, "a/b", 429, "quota_exhausted", 3600, "reserve reached"},
		{"G3 unknown reset re-checks hourly", "key_unknown_reset.json", credentials{apiKey: exampleKey, reserve: 5}, "a/b", 429, "quota_exhausted", 3600, "reserve reached"},
		{"G3 effective budget binds before an unlimited key", "key_budgets_mixed.json", credentials{apiKey: exampleKey, allowUnlimited: true, reserve: 3}, "a/b", 429, "quota_exhausted", secondsUntil("2026-11-05T00:00:00Z"), "reserve reached"},
		{"G4 free daily requests used up", "key_free_tier.json", key, "vendor/model:free", 429, "quota_exhausted", secondsUntil("2026-11-05T00:00:00Z"), "free-model requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, fixture(t, tc.file))
			e := rpcErr(t, admitWith(h, tc.c, tc.model))
			if e.HTTPStatus != tc.status || e.Code != tc.code || e.RetryAfterSeconds != tc.retry || !strings.Contains(e.Message, tc.message) || !strings.HasPrefix(e.Message, tc.code+": ") {
				t.Fatalf("%+v", e)
			}
		})
	}
}

func TestGuardAdmits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		c     credentials
		model string
	}{
		{"monthly key with room", "key_monthly_limit.json", credentials{apiKey: exampleKey}, "a/b"},
		{"no limit when explicitly allowed", "key_no_limit.json", credentials{apiKey: exampleKey, allowUnlimited: true}, "a/b"},
		{"paid model while free requests are used up", "key_free_tier.json", credentials{apiKey: exampleKey}, "vendor/model"},
		{"free model with requests left", "key_free_paid.json", credentials{apiKey: exampleKey}, "vendor/model:free"},
		{"unknown fields ignored", "key_extra_unknown_fields.json", credentials{apiKey: exampleKey}, "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := admitWith(newHarness(t, fixture(t, tc.file)), tc.c, tc.model); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGuardReserveBoundary(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json")) // 10.5 remaining
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey, reserve: 10.5}, "a/b")); e.Code != "quota_exhausted" {
		t.Fatalf("remaining == reserve must refuse: %+v", e)
	}
	if err := admitWith(h, credentials{apiKey: exampleKey, reserve: 10.49}, "a/b"); err != nil {
		t.Fatalf("reserve + 0.01 must admit: %v", err)
	}
}

func TestGuardBindingIsTheMinimumLayer(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json")) // key 10.5 monthly
	h.srv.credits = []byte(`{"data":{"total_credits":100,"total_usage":99.5}}`)
	c := credentials{apiKey: exampleKey, reserve: 1}
	// Only the account balance (0.5) is under the reserve: no reset, hourly re-check.
	if e := rpcErr(t, admitWith(h, c, "a/b")); e.Code != "quota_exhausted" || e.RetryAfterSeconds != 3600 || !strings.Contains(e.Message, "reserve") {
		t.Fatalf("account layer: %+v", e)
	}
	// Key and account both block: wait for the later of their resets.
	h = newHarness(t, fixture(t, "key_monthly_limit.json"))
	h.srv.credits = fixture(t, "credits_ok.json") // balance 74.75
	c.reserve = 80
	if e := rpcErr(t, admitWith(h, c, "a/b")); e.RetryAfterSeconds != secondsUntil("2026-12-01T00:00:00Z") {
		t.Fatalf("both layers: %+v", e)
	}
	c.reserve = 10
	if err := admitWith(h, c, "a/b"); err != nil {
		t.Fatalf("all layers above reserve: %v", err)
	}
	for _, seen := range h.srv.seen() {
		if seen != "/credits Bearer "+exampleKey && seen != "/key Bearer "+exampleKey {
			t.Fatalf("key sent to the wrong endpoint: %q", seen)
		}
	}
}

func TestGuardLimitWithoutRemainingFailsClosed(t *testing.T) {
	h := newHarness(t, []byte(`{"data":{"limit":5,"limit_remaining":null,"limit_reset":"daily"}}`))
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey}, "a/b")); e.Code != "quota_unverified" || e.HTTPStatus != 429 || e.RetryAfterSeconds != 60 {
		t.Fatalf("%+v", e)
	}
}

func TestGuardFailsClosedWithoutReading(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.set([]byte(`{"error":{"code":500}}`), 500)
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey}, "a/b")); e.Code != "quota_unverified" || e.HTTPStatus != 429 || e.RetryAfterSeconds != 60 || e.Message != "quota_unverified: OpenRouter key status could not be read" {
		t.Fatalf("G0: %+v", e)
	}
	h.srv.set(fixture(t, "key_401.json"), 401)
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey}, "a/b")); e.Code != "invalid_credentials" || e.HTTPStatus != 401 {
		t.Fatalf("revoked key: %+v", e)
	}
}

func TestGuardRefusesBeforeHostProxyIsKnown(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	h.guard = newGuard(h.keys) // host proxy never observed
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey}, "a/b")); e.Code != "quota_unverified" || h.srv.keyCalls.Load() != 0 {
		t.Fatalf("%+v calls=%d", e, h.srv.keyCalls.Load())
	}
	// An auth-level proxy needs no host setting ("direct" bypasses proxies).
	if err := h.guard.admit(context.Background(), "auth-1", credentials{apiKey: exampleKey}, storage(`,"proxy_url":"direct"`), nil, "a/b"); err != nil {
		t.Fatal(err)
	}
}

func TestGuardLastGoodFallback(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	c := credentials{apiKey: exampleKey}
	if err := admitWith(h, c, "a/b"); err != nil {
		t.Fatal(err)
	}
	h.srv.set([]byte(`{"error":{"code":503}}`), 503)
	h.clock.advance(61 * time.Second)
	if err := admitWith(h, c, "a/b"); err != nil {
		t.Fatalf("fresh lastGood after a failed re-read must admit: %v", err)
	}
	h.clock.advance(8*time.Minute + 58*time.Second) // 9:59 old
	if err := admitWith(h, c, "a/b"); err != nil {
		t.Fatalf("9:59-old lastGood must admit: %v", err)
	}
	h.clock.advance(2 * time.Second) // 10:01 old
	if e := rpcErr(t, admitWith(h, c, "a/b")); e.Code != "quota_unverified" {
		t.Fatalf("10:01-old lastGood must refuse: %+v", e)
	}
	if calls := h.srv.keyCalls.Load(); calls != 4 {
		t.Fatalf("each stale admission must re-read: %d", calls)
	}
}

func TestGuardInvalidatedReadingIsNoFallback(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	c := credentials{apiKey: exampleKey}
	if err := admitWith(h, c, "a/b"); err != nil {
		t.Fatal(err)
	}
	h.srv.set([]byte(`{"error":{"code":503}}`), 503)
	h.keys.invalidate("auth-1")
	if e := rpcErr(t, admitWith(h, c, "a/b")); e.Code != "quota_unverified" || h.srv.keyCalls.Load() != 2 {
		t.Fatalf("%+v calls=%d", e, h.srv.keyCalls.Load())
	}
}

func TestGuardSingleFlight(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	gate := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.gate = gate
	h.srv.mu.Unlock()
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if admitWith(h, credentials{apiKey: exampleKey}, "a/b") != nil {
				failures.Add(1)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.srv.keyCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if h.srv.keyCalls.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("calls=%d failures=%d", h.srv.keyCalls.Load(), failures.Load())
	}
}

func TestGuardFreeModelCooldownIsPerModel(t *testing.T) {
	h := newHarness(t, fixture(t, "key_free_tier.json"))
	c := credentials{apiKey: exampleKey}
	e := rpcErr(t, admitWith(h, c, "meta-llama/llama-3.3-70b-instruct:free"))
	if e.Code != "quota_exhausted" || e.RetryAfterSeconds != secondsUntil("2026-11-05T00:00:00Z") {
		t.Fatalf("%+v", e)
	}
	if err := admitWith(h, c, "anthropic/claude-sonnet-4.5"); err != nil {
		t.Fatalf("paid model on the same key: %v", err)
	}
}

func TestGuardUsesObservedHostProxy(t *testing.T) {
	var seen atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/key") {
			seen.Store(r.URL.String())
		}
		_, _ = w.Write(fixture(t, "key_monthly_limit.json"))
	}))
	defer proxy.Close()
	client := kit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	keys := newKeyState(client)
	keys.base = "http://openrouter.invalid/api/v1"
	g := newGuard(keys)
	g.observeHost(pluginapi.HostConfigSummary{ProxyURL: proxy.URL})
	if err := g.admit(context.Background(), "auth-1", credentials{apiKey: exampleKey}, storage(""), nil, "a/b"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(seen.Load()); got != "http://openrouter.invalid/api/v1/key" {
		t.Fatalf("host proxy not used: %q", got)
	}
}
