package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

var keyFixtures = []string{"key_monthly_limit.json", "key_daily_limit_no_budgets.json", "key_weekly_limit.json", "key_lifetime_limit.json", "key_no_limit.json", "key_exhausted_negative.json", "key_zero_limit.json", "key_unknown_reset.json", "key_budgets_mixed.json", "key_free_tier.json", "key_free_paid.json", "key_expiring.json", "key_wrong_types.json", "key_extra_unknown_fields.json"}

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err = os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden:\n%s", name, got)
	}
}

func TestNormalizeQuotaGolden(t *testing.T) {
	for _, file := range keyFixtures {
		r, err := parseKey(fixture(t, file), now0)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "quota_"+file, normalizeQuota(r, credentials{apiKey: exampleKey}, now0))
	}
	r, _ := parseKey(fixture(t, "key_no_limit.json"), now0)
	golden(t, "quota_key_no_limit_allowed.json", normalizeQuota(r, credentials{apiKey: exampleKey, allowUnlimited: true, reserve: 2}, now0))
	r, _ = parseKey(fixture(t, "key_monthly_limit.json"), now0)
	a, _ := parseCredits(fixture(t, "credits_ok.json"))
	r.Account = a
	golden(t, "quota_key_monthly_with_credits.json", normalizeQuota(r, credentials{apiKey: exampleKey}, now0))
}

func TestNormalizeQuotaInvariants(t *testing.T) {
	for _, file := range keyFixtures {
		r, _ := parseKey(fixture(t, file), now0)
		out := normalizeQuota(r, credentials{apiKey: exampleKey}, now0)
		for _, g := range out.Groups {
			for _, b := range g.Buckets {
				if b.RemainingFraction < 0 || b.RemainingFraction > 1 {
					t.Errorf("%s: fraction %v", file, b.RemainingFraction)
				}
				if b.ResetTime != "" {
					if reset, err := time.Parse(time.RFC3339, b.ResetTime); err != nil || !reset.After(now0) || !strings.HasSuffix(b.ResetTime, "Z") {
						t.Errorf("%s: reset %q", file, b.ResetTime)
					}
				}
				if strings.Contains(b.Description, exampleKey) {
					t.Errorf("%s: key in description", file)
				}
			}
		}
		for _, m := range out.Summary {
			if strings.HasSuffix(m.Key, "_usd") && (m.Format != "currency" || m.Currency != "USD") {
				t.Errorf("%s: %+v", file, m)
			}
		}
	}
}

func TestNoLimitBucketModes(t *testing.T) {
	r, _ := parseKey(fixture(t, "key_no_limit.json"), now0)
	refused := normalizeQuota(r, credentials{apiKey: exampleKey}, now0).Groups[0].Buckets[0]
	allowed := normalizeQuota(r, credentials{apiKey: exampleKey, allowUnlimited: true}, now0).Groups[0].Buckets[0]
	if refused.RemainingFraction != 0 || !strings.Contains(refused.Window, "plugin refuses requests") || allowed.RemainingFraction != 0 || !strings.Contains(allowed.Window, "UNBOUNDED") {
		t.Fatalf("%+v %+v", refused, allowed)
	}
}

func quotaRequest(extra string) pluginapi.QuotaFetchRequest {
	return pluginapi.QuotaFetchRequest{AuthID: "auth-1", Provider: provider, StorageJSON: storage(extra)}
}

func TestQuotaProviderFetch(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	out, err := h.quota.FetchQuota(context.Background(), quotaRequest(""))
	// The harness serves no /credits body, so the balance is omitted.
	if err != nil || len(out.Groups) != 2 || out.Groups[0].Buckets[0].RemainingFraction != 0.42 || h.srv.creditCalls.Load() != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	desc, _ := h.quota.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if desc.DisplayName != "OpenRouter" || desc.SupportsReset || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != provider {
		t.Fatalf("%+v", desc)
	}
}

// GET /credits uses the regular key. Any failure omits only the account
// group; the key limit and everything else still work.
func TestQuotaProviderCredits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		groups int
	}{
		{"success adds the account group", fixture(t, "credits_ok.json"), 200, 3},
		{"live regular-key response adds the account group", fixture(t, "live_credits_inference.json"), 200, 3},
		{"401 omits only the account group", fixture(t, "key_401.json"), 401, 2},
		{"403 omits only the account group", fixture(t, "credits_403.json"), 403, 2},
		{"500 omits only the account group", []byte(`{"error":{"code":500}}`), 500, 2},
		{"invalid body omits only the account group", []byte(`{"data":{"total_credits":"x"}}`), 200, 2},
		{"zero credits", fixture(t, "credits_zero.json"), 200, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, fixture(t, "key_monthly_limit.json"))
			h.srv.credits, h.srv.creditsCode = tc.body, tc.status
			out, err := h.quota.FetchQuota(context.Background(), quotaRequest(""))
			if err != nil || len(out.Groups) != tc.groups || h.srv.creditCalls.Load() != 1 || out.Groups[0].DisplayName != "Key credit limit" {
				t.Fatalf("%+v %v", out, err)
			}
			if tc.groups == 3 {
				b := out.Groups[2].Buckets[0]
				if out.Groups[2].DisplayName != "Account credits" || b.ResetTime != "" || b.RemainingFraction < 0 || b.RemainingFraction > 1 {
					t.Fatalf("%+v", out.Groups[2])
				}
			}
			for _, seen := range h.srv.seen() {
				if seen != "/credits Bearer "+exampleKey && seen != "/key Bearer "+exampleKey {
					t.Fatalf("unexpected request %q", seen)
				}
			}
		})
	}
}

// A slow /credits only delays a read by the credits timeout, then the
// balance is omitted.
func TestSlowCreditsDoesNotHoldAdmission(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credits") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write(fixture(t, "key_monthly_limit.json"))
	}))
	defer srv.Close()
	defer close(release)
	client := kit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	keys := newKeyState(client)
	keys.base, keys.creditsTimeout = srv.URL, 50*time.Millisecond
	start := time.Now()
	r, err := keys.get(context.Background(), "auth-1", credentials{apiKey: exampleKey}, 0)
	if err != nil || r.Account != nil || r.Limit == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("read waited %v for /credits", took)
	}
}

func TestQuotaProviderManagementKeyError(t *testing.T) {
	h := newHarness(t, fixture(t, "key_management.json"))
	_, err := h.quota.FetchQuota(context.Background(), quotaRequest(""))
	if e := rpcErr(t, err); e.Code != "invalid_credentials" || !strings.Contains(e.Message, "management key") {
		t.Fatalf("%+v", e)
	}
}

func TestQuotaProviderCacheSingleFlightAndErrors(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	gate := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.gate = gate
	h.srv.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.quota.FetchQuota(context.Background(), quotaRequest("")); err != nil {
				t.Error(err)
			}
		}()
	}
	for h.srv.keyCalls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	h.srv.mu.Lock()
	h.srv.gate = nil
	h.srv.mu.Unlock()
	if _, err := h.quota.FetchQuota(context.Background(), quotaRequest("")); err != nil || h.srv.keyCalls.Load() != 1 {
		t.Fatalf("cached poll re-read: %d %v", h.srv.keyCalls.Load(), err)
	}
	// Invalidation drops the cached card and the key reading.
	h.keys.invalidate("auth-1")
	h.srv.set([]byte(`{"error":{"code":503}}`), 503)
	if _, err := h.quota.FetchQuota(context.Background(), quotaRequest("")); err == nil || h.srv.keyCalls.Load() != 2 {
		t.Fatalf("invalidate did not force a re-read: %d %v", h.srv.keyCalls.Load(), err)
	}
	// Errors are never cached.
	h.srv.set(fixture(t, "key_weekly_limit.json"), 200)
	out, err := h.quota.FetchQuota(context.Background(), quotaRequest(""))
	if err != nil || h.srv.keyCalls.Load() != 3 || out.Groups[0].Buckets[0].Window != "weekly key limit" {
		t.Fatalf("%+v %v calls=%d", out, err, h.srv.keyCalls.Load())
	}
}

func TestQuotaProviderSharesGuardReading(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	if err := admitWith(h, credentials{apiKey: exampleKey}, "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.quota.FetchQuota(context.Background(), quotaRequest("")); err != nil || h.srv.keyCalls.Load() != 1 {
		t.Fatalf("quota card re-read a fresh guard reading: %d %v", h.srv.keyCalls.Load(), err)
	}
}

func TestInFlightPollNeverOverwritesNewerReading(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credits") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if calls.Add(1) == 1 {
			<-release
			_, _ = w.Write(fixture(t, "key_weekly_limit.json"))
			return
		}
		_, _ = w.Write(fixture(t, "key_monthly_limit.json"))
	}))
	defer srv.Close()
	client := kit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	keys := newKeyState(client)
	keys.base = srv.URL
	c := credentials{apiKey: exampleKey}
	done := make(chan keyReading)
	go func() {
		r, _ := keys.get(context.Background(), "auth-1", c, 0)
		done <- r
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	keys.invalidate("auth-1") // e.g. an upstream 403 arrived meanwhile
	newer, err := keys.get(context.Background(), "auth-1", c, 0)
	if err != nil || newer.Reset != windowMonthly {
		t.Fatalf("%+v %v", newer, err)
	}
	close(release)
	// The first caller's read started before the invalidation, so it must not
	// be returned: the caller takes the newer reading instead.
	if old := <-done; old.Reset != windowMonthly {
		t.Fatalf("a read overtaken by an invalidation was returned: %v", old.Reset)
	}
	if got := keys.peek("auth-1"); got == nil || got.Reset != windowMonthly {
		t.Fatal("an older in-flight read replaced the newer reading")
	}
	if r, _ := keys.get(context.Background(), "auth-1", c, 0); r.Reset != windowMonthly || calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestCredentialChangeStartsOver(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	if _, err := h.keys.get(context.Background(), "auth-1", credentials{apiKey: exampleKey}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.keys.get(context.Background(), "auth-1", credentials{apiKey: "rotated-key"}, 0); err != nil || h.srv.keyCalls.Load() != 2 {
		t.Fatalf("a rotated key reused the old reading: %d %v", h.srv.keyCalls.Load(), err)
	}
}
