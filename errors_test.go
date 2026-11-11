package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func readingFrom(t *testing.T, file string) *keyReading {
	r, err := parseKey(fixture(t, file), now0)
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestClassifyFailureTable(t *testing.T) {
	monthly := readingFrom(t, "key_monthly_limit.json")
	daily := readingFrom(t, "key_daily_limit_no_budgets.json")
	lifetime := readingFrom(t, "key_lifetime_limit.json")
	freeUsed := readingFrom(t, "key_free_tier.json")
	freeLeft := readingFrom(t, "key_free_paid.json")
	nov5, nov9, dec1 := secondsUntil("2026-11-05T00:00:00Z"), secondsUntil("2026-11-09T00:00:00Z"), secondsUntil("2026-12-01T00:00:00Z")
	ms := strconv.FormatInt(now0.Add(30e9).UnixMilli(), 10)
	sec := strconv.FormatInt(now0.Add(45e9).Unix(), 10)
	for _, tc := range []struct {
		file       string
		status     int
		headers    map[string]string
		reading    *keyReading
		model      string
		wantStatus int
		code       string
		retry      int
		effect     effect
		scoped     bool
	}{
		{"err_402_credits.json", 402, nil, monthly, "a/b", 429, "quota_exhausted", 3600, effectCredits, true},
		{"err_402_weight.json", 402, nil, monthly, "a/b", 400, "request_exceeds_budget", 0, effectReread, false},
		{"err_402_inflight.json", 402, map[string]string{"Retry-After": "7"}, monthly, "a/b", 429, "rate_limited", 7, effectReread, false},
		{"err_402_inflight.json", 402, nil, monthly, "a/b", 429, "rate_limited", 5, effectReread, false},
		{"err_402_key_limit.json", 402, nil, monthly, "a/b", 429, "quota_exhausted", dec1, effectKeyLimit, true},
		{"err_402_key_limit.json", 402, nil, nil, "a/b", 429, "quota_exhausted", 3600, effectKeyLimit, true},
		{"err_403_key_limit.json", 403, nil, monthly, "a/b", 429, "quota_exhausted", nov5, effectKeyLimit, true},
		{"err_403_key_limit_no_reset.json", 403, nil, daily, "a/b", 429, "quota_exhausted", nov5, effectKeyLimit, true},
		{"err_403_key_limit_no_reset.json", 403, nil, lifetime, "a/b", 429, "quota_exhausted", 3600, effectKeyLimit, true},
		{"err_403_guardrail_weekly.json", 403, nil, nil, "a/b", 429, "quota_exhausted", nov9, effectBudget, true},
		{"err_403_workspace_lifetime.json", 403, nil, nil, "a/b", 429, "quota_exhausted", 3600, effectBudget, true},
		{"err_403_other_limit.json", 403, nil, nil, "a/b", 429, "quota_exhausted", 3600, effectLimit, true},
		{"err_403_moderation.json", 403, nil, nil, "a/b", 400, "content_policy", 0, effectNone, false},
		{"err_403_permission.json", 403, nil, nil, "a/b", 403, "entitlement_required", 0, effectNone, false},
		{"err_429_platform_ms.json", 429, map[string]string{"X-RateLimit-Limit": "20", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": ms}, nil, "a/b", 429, "rate_limited", 30, effectNone, false},
		{"err_429_platform_s.json", 429, map[string]string{"X-RateLimit-Limit": "20", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": sec}, nil, "a/b", 429, "rate_limited", 45, effectNone, false},
		{"err_429_platform_s.json", 429, map[string]string{"X-RateLimit-Reset": "12"}, nil, "a/b", 429, "rate_limited", 12, effectNone, false},
		{"err_429_provider_retry_after.json", 429, map[string]string{"Retry-After": "9"}, nil, "a/b", 429, "rate_limited", 9, effectNone, false},
		{"err_429_provider_retry_after.json", 429, nil, nil, "a/b", 429, "rate_limited", 0, effectNone, false},
		{"err_429_free.json", 429, nil, freeUsed, "vendor/model:free", 429, "quota_exhausted", nov5, effectFree, false},
		// The free-models-per-day message wins over lagging counters.
		{"err_429_free.json", 429, nil, freeLeft, "vendor/model:free", 429, "quota_exhausted", nov5, effectFree, false},
		{"err_429_free.json", 429, nil, nil, "vendor/model:free", 429, "quota_exhausted", nov5, effectFree, false},
		{"err_429_free.json", 429, map[string]string{"Retry-After": "90000"}, nil, "vendor/model:free", 429, "quota_exhausted", 90000, effectFree, false},
		{"err_429_provider_retry_after.json", 429, nil, freeUsed, "vendor/model:free", 429, "quota_exhausted", nov5, effectFree, false},
		{"err_429_provider_retry_after.json", 429, nil, freeLeft, "vendor/model:free", 429, "rate_limited", 0, effectNone, false},
		{"err_403_inflight.json", 403, nil, monthly, "a/b", 429, "rate_limited", 5, effectReread, false},
		{"live_err_503_free_no_upstreams.json", 503, nil, freeLeft, "vendor/model:free", 503, "upstream_unavailable", 0, effectNone, false},
		{"err_401_expired.json", 401, nil, nil, "a/b", 401, "invalid_credentials", 0, effectNone, false},
		{"err_404.json", 404, nil, nil, "a/b", 404, "upstream_not_found", 0, effectNone, false},
		{"err_408.json", 408, nil, nil, "a/b", 408, "timeout", 0, effectNone, false},
		{"err_408.json", 408, map[string]string{"Retry-After": "3"}, nil, "a/b", 408, "timeout", 3, effectNone, false},
		{"err_502.json", 502, nil, nil, "a/b", 502, "upstream_unavailable", 0, effectNone, false},
		{"err_503.json", 503, map[string]string{"Retry-After": "11"}, nil, "a/b", 503, "upstream_unavailable", 11, effectNone, false},
		{"err_524.json", 524, nil, nil, "a/b", 524, "upstream_unavailable", 0, effectNone, false},
		{"err_529.json", 529, nil, nil, "a/b", 529, "upstream_unavailable", 0, effectNone, false},
		{"err_502_html.txt", 502, nil, nil, "a/b", 502, "upstream_error", 0, effectNone, false},
		{"err_502_html.txt", 400, nil, nil, "a/b", 400, "invalid_request", 0, effectNone, false},
		{"err_502_html.txt", 413, nil, nil, "a/b", 413, "invalid_request", 0, effectNone, false},
		{"err_502_html.txt", 422, nil, nil, "a/b", 422, "invalid_request", 0, effectNone, false},
		{"err_502_html.txt", 418, nil, nil, "a/b", 418, "upstream_error", 0, effectNone, false},
	} {
		h := http.Header{}
		for k, v := range tc.headers {
			h.Set(k, v)
		}
		body := fixture(t, tc.file)
		e, eff := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Headers: h, Body: body}, now0, tc.reading, tc.model)
		name := tc.file + " " + strconv.Itoa(tc.status) + " " + tc.model
		if e.HTTPStatus != tc.wantStatus || e.Code != tc.code || e.RetryAfterSeconds != tc.retry || eff != tc.effect || e.CredentialScoped != tc.scoped {
			t.Errorf("%s: got (%d, %s, %d, %v, scoped=%v) want (%d, %s, %d, %v, scoped=%v)", name, e.HTTPStatus, e.Code, e.RetryAfterSeconds, eff, e.CredentialScoped, tc.wantStatus, tc.code, tc.retry, tc.effect, tc.scoped)
		}
		if !strings.HasPrefix(e.Message, e.Code+": ") || strings.Contains(e.Message, exampleKey) || strings.Contains(e.Message, "exceeded") || strings.Contains(e.Message, "html") || strings.Contains(e.Message, "upstreams") {
			t.Errorf("%s: unsafe or unprefixed message %q", name, e.Message)
		}
	}
}

func TestNotFoundIsAModelSupportFailure(t *testing.T) {
	e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: 404, Body: fixture(t, "err_404.json")}, now0, nil, "a/b")
	if !strings.Contains(e.Message, "model_not_supported") {
		t.Fatalf("host model-support marker missing: %q", e.Message)
	}
}

func TestExhaustionRetryAfterOnlyExtends(t *testing.T) {
	body := fixture(t, "err_403_key_limit.json") // resets 2026-11-05: 30600 s
	for header, want := range map[string]int{"10": 30600, "40000": 40000} {
		e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: 403, Headers: http.Header{"Retry-After": {header}}, Body: body}, now0, nil, "a/b")
		if e.RetryAfterSeconds != want {
			t.Errorf("Retry-After %s: %d want %d", header, e.RetryAfterSeconds, want)
		}
	}
}

// Account-wide quota branches carry CredentialScoped with the exact
// reset-derived retry hint under pinned now; every sibling keeps its prior
// shape (Code, Message, 429, retry) with the flag clear.
func TestClassifyFailureCredentialScope(t *testing.T) {
	monthly := readingFrom(t, "key_monthly_limit.json")
	freeUsed := readingFrom(t, "key_free_tier.json")
	scoped := []struct {
		name    string
		file    string
		status  int
		reading *keyReading
		model   string
		retry   int
	}{
		{"key limit resets_at", "err_403_key_limit.json", 403, nil, "a/b", secondsUntil("2026-11-05T00:00:00Z")},
		{"key limit window", "err_402_key_limit.json", 402, monthly, "a/b", secondsUntil("2026-12-01T00:00:00Z")},
		{"guardrail budget", "err_403_guardrail_weekly.json", 403, nil, "a/b", secondsUntil("2026-11-09T00:00:00Z")},
		{"workspace budget", "err_403_workspace_lifetime.json", 403, nil, "a/b", 3600},
		{"credits re-check", "err_402_credits.json", 402, monthly, "a/b", 3600},
		{"other spend limit", "err_403_other_limit.json", 403, nil, "a/b", 3600},
	}
	for _, tc := range scoped {
		e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: fixture(t, tc.file)}, now0, tc.reading, tc.model)
		if !e.CredentialScoped || e.Code != "quota_exhausted" || e.HTTPStatus != 429 || e.RetryAfterSeconds != tc.retry {
			t.Errorf("%s: %+v", tc.name, e)
		}
	}
	negative := []struct {
		name   string
		file   string
		status int
		model  string
	}{
		{"free daily quota", "err_429_free.json", 429, "vendor/model:free"},
		{"transient inflight", "err_402_inflight.json", 402, "a/b"},
		{"request-scoped weight", "err_402_weight.json", 402, "a/b"},
		{"per-model rate limit", "err_429_platform_ms.json", 429, "a/b"},
		{"non-limit 5xx", "err_503.json", 503, "a/b"},
	}
	for _, tc := range negative {
		e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: fixture(t, tc.file)}, now0, freeUsed, tc.model)
		if e.CredentialScoped {
			t.Errorf("%s: must stay unscoped: %+v", tc.name, e)
		}
	}
}

func TestErrorBodyParseIsCapped(t *testing.T) {
	big := `{"error":{"code":402,"metadata":{"limit_source":"openrouter_in_flight_budget"},"pad":"` + strings.Repeat("x", maxErrorBody) + `"}}`
	e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: 402, Body: []byte(big)}, now0, nil, "a/b")
	// Past 64 KiB the body is unreadable, so the 402 is treated as credits.
	if e.Code != "quota_exhausted" {
		t.Fatalf("%+v", e)
	}
}

// Every limit-related failure invalidates the reading, so the next admission
// re-reads. A spent cap also latches the auth: the re-read still shows
// headroom (the counters lag), yet the next admission is refused.
func TestEachExhaustionInvalidatesKeyState(t *testing.T) {
	for file, latched := range map[string]bool{"err_402_credits.json": true, "err_402_key_limit.json": true, "err_403_key_limit.json": true, "err_403_guardrail_weekly.json": true, "err_403_workspace_lifetime.json": true, "err_402_inflight.json": false, "err_403_inflight.json": false, "err_402_weight.json": false} {
		h := newHarness(t, fixture(t, "key_monthly_limit.json"))
		if err := admitWith(h, credentials{apiKey: exampleKey}, "a/b"); err != nil {
			t.Fatal(err)
		}
		var dropped []string
		h.keys.onInvalidate = func(id string) { dropped = append(dropped, id) }
		status := 403
		if strings.HasPrefix(file, "err_402") {
			status = 402
		}
		_ = h.exec.classify("auth-1", "a/b", pluginapi.HTTPResponse{StatusCode: status, Body: fixture(t, file)})
		if len(dropped) != 1 || dropped[0] != "auth-1" {
			t.Fatalf("%s: invalidations %v", file, dropped)
		}
		err := admitWith(h, credentials{apiKey: exampleKey}, "c/d")
		if h.srv.keyCalls.Load() != 2 {
			t.Fatalf("%s: the next admission must re-read (calls=%d)", file, h.srv.keyCalls.Load())
		}
		if latched {
			// The latch refusal is the same account-wide exhaustion the
			// classify branch scoped: the host must see the same pair.
			if e := rpcErr(t, err); e.Code != "quota_exhausted" || e.RetryAfterSeconds <= 0 || !e.CredentialScoped {
				t.Fatalf("%s: a spent cap must latch the auth and stay credential-scoped: %+v", file, e)
			}
		} else if err != nil {
			t.Fatalf("%s: a transient limit must not latch: %v", file, err)
		}
	}
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	called := false
	h.keys.onInvalidate = func(string) { called = true }
	for _, file := range []string{"err_503.json", "live_err_503_free_no_upstreams.json"} {
		_ = h.exec.classify("auth-1", "vendor/model:free", pluginapi.HTTPResponse{StatusCode: 503, Body: fixture(t, file)})
	}
	if called {
		t.Fatal("an outage must not invalidate the key reading")
	}
}
