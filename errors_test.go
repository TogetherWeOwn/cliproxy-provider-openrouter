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
		invalidate bool
	}{
		{"err_402_credits.json", 402, nil, monthly, "a/b", 429, "quota_exhausted", 3600, true},
		{"err_402_weight.json", 402, nil, monthly, "a/b", 400, "request_exceeds_budget", 0, true},
		{"err_402_inflight.json", 402, map[string]string{"Retry-After": "7"}, monthly, "a/b", 429, "rate_limited", 7, true},
		{"err_402_inflight.json", 402, nil, monthly, "a/b", 429, "rate_limited", 5, true},
		{"err_402_key_limit.json", 402, nil, monthly, "a/b", 429, "quota_exhausted", dec1, true},
		{"err_402_key_limit.json", 402, nil, nil, "a/b", 429, "quota_exhausted", 3600, true},
		{"err_403_key_limit.json", 403, nil, monthly, "a/b", 429, "quota_exhausted", nov5, true},
		{"err_403_key_limit_no_reset.json", 403, nil, daily, "a/b", 429, "quota_exhausted", nov5, true},
		{"err_403_key_limit_no_reset.json", 403, nil, lifetime, "a/b", 429, "quota_exhausted", 3600, true},
		{"err_403_guardrail_weekly.json", 403, nil, nil, "a/b", 429, "quota_exhausted", nov9, true},
		{"err_403_workspace_lifetime.json", 403, nil, nil, "a/b", 429, "quota_exhausted", 3600, true},
		{"err_403_moderation.json", 403, nil, nil, "a/b", 400, "content_policy", 0, false},
		{"err_403_permission.json", 403, nil, nil, "a/b", 403, "entitlement_required", 0, false},
		{"err_429_platform_ms.json", 429, map[string]string{"X-RateLimit-Limit": "20", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": ms}, nil, "a/b", 429, "rate_limited", 30, false},
		{"err_429_platform_s.json", 429, map[string]string{"X-RateLimit-Limit": "20", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": sec}, nil, "a/b", 429, "rate_limited", 45, false},
		{"err_429_platform_s.json", 429, map[string]string{"X-RateLimit-Reset": "12"}, nil, "a/b", 429, "rate_limited", 12, false},
		{"err_429_provider_retry_after.json", 429, map[string]string{"Retry-After": "9"}, nil, "a/b", 429, "rate_limited", 9, false},
		{"err_429_provider_retry_after.json", 429, nil, nil, "a/b", 429, "rate_limited", 0, false},
		{"err_429_free.json", 429, nil, freeUsed, "vendor/model:free", 429, "quota_exhausted", nov5, true},
		{"err_429_free.json", 429, nil, freeLeft, "vendor/model:free", 429, "rate_limited", 0, false},
		{"err_401_expired.json", 401, nil, nil, "a/b", 401, "invalid_credentials", 0, false},
		{"err_404.json", 404, nil, nil, "a/b", 404, "upstream_not_found", 0, false},
		{"err_408.json", 408, nil, nil, "a/b", 408, "timeout", 0, false},
		{"err_408.json", 408, map[string]string{"Retry-After": "3"}, nil, "a/b", 408, "timeout", 3, false},
		{"err_502.json", 502, nil, nil, "a/b", 502, "upstream_unavailable", 0, false},
		{"err_503.json", 503, map[string]string{"Retry-After": "11"}, nil, "a/b", 503, "upstream_unavailable", 11, false},
		{"err_524.json", 524, nil, nil, "a/b", 524, "upstream_unavailable", 0, false},
		{"err_529.json", 529, nil, nil, "a/b", 529, "upstream_unavailable", 0, false},
		{"err_502_html.txt", 502, nil, nil, "a/b", 502, "upstream_error", 0, false},
		{"err_502_html.txt", 400, nil, nil, "a/b", 400, "invalid_request", 0, false},
		{"err_502_html.txt", 413, nil, nil, "a/b", 413, "invalid_request", 0, false},
		{"err_502_html.txt", 422, nil, nil, "a/b", 422, "invalid_request", 0, false},
		{"err_502_html.txt", 418, nil, nil, "a/b", 418, "upstream_error", 0, false},
	} {
		h := http.Header{}
		for k, v := range tc.headers {
			h.Set(k, v)
		}
		body := fixture(t, tc.file)
		e, invalidate := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Headers: h, Body: body}, now0, tc.reading, tc.model)
		name := tc.file + " " + strconv.Itoa(tc.status)
		if e.HTTPStatus != tc.wantStatus || e.Code != tc.code || e.RetryAfterSeconds != tc.retry || invalidate != tc.invalidate {
			t.Errorf("%s: got (%d, %s, %d, %v) want (%d, %s, %d, %v)", name, e.HTTPStatus, e.Code, e.RetryAfterSeconds, invalidate, tc.wantStatus, tc.code, tc.retry, tc.invalidate)
		}
		if !strings.HasPrefix(e.Message, e.Code+": ") || strings.Contains(e.Message, exampleKey) || strings.Contains(e.Message, "exceeded") || strings.Contains(e.Message, "html") {
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

func TestErrorBodyParseIsCapped(t *testing.T) {
	big := `{"error":{"code":402,"metadata":{"limit_source":"openrouter_in_flight_budget"},"pad":"` + strings.Repeat("x", maxErrorBody) + `"}}`
	e, _ := classifyFailure(pluginapi.HTTPResponse{StatusCode: 402, Body: []byte(big)}, now0, nil, "a/b")
	// Past 64 KiB the body is unreadable, so the 402 is treated as credits.
	if e.Code != "quota_exhausted" {
		t.Fatalf("%+v", e)
	}
}

func TestEachExhaustionInvalidatesKeyState(t *testing.T) {
	for _, file := range []string{"err_402_credits.json", "err_402_key_limit.json", "err_403_key_limit.json", "err_403_guardrail_weekly.json", "err_403_workspace_lifetime.json", "err_402_inflight.json", "err_402_weight.json"} {
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
		if err := admitWith(h, credentials{apiKey: exampleKey}, "a/b"); err != nil || h.srv.keyCalls.Load() != 2 {
			t.Fatalf("%s: the next admission must re-read (calls=%d, err=%v)", file, h.srv.keyCalls.Load(), err)
		}
	}
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	called := false
	h.keys.onInvalidate = func(string) { called = true }
	_ = h.exec.classify("auth-1", "a/b", pluginapi.HTTPResponse{StatusCode: 503, Body: fixture(t, "err_503.json")})
	if called {
		t.Fatal("an outage must not invalidate the key reading")
	}
}
