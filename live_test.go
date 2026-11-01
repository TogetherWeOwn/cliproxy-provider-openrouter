package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// liveNow is when the redacted live responses in testdata/live_*.json were
// captured: Sunday 2026-11-01 09:58 UTC.
var liveNow = time.Date(2026, 11, 1, 9, 58, 0, 0, time.UTC)

// liveBalance is total_credits - total_usage of the live /credits response,
// computed in float64 as the plugin does.
var liveCredits, liveUsage = 245.0, 237.537304708

func liveBalance() float64 { return liveCredits - liveUsage }

// liveHarness serves the live GET /key and GET /credits responses.
func liveHarness(t *testing.T) *harness {
	h := newHarness(t, fixture(t, "live_key_inference.json"))
	h.srv.credits = fixture(t, "live_credits_inference.json")
	h.clock = newClock(liveNow)
	h.keys.now, h.guard.now, h.exec.now = h.clock.now, h.clock.now, h.clock.now
	return h
}

func TestParseLiveKey(t *testing.T) {
	f := ptr[float64]
	nov2 := utc("2026-11-02T00:00:00Z")
	keyLimit := budget{Scope: "key_limit", Limit: f(5), Spend: f(0), Remaining: f(5), Interval: windowWeekly, ResetsAt: &nov2}
	want := keyReading{FetchedAt: liveNow, Limit: f(5), Remaining: f(5), Reset: windowWeekly, KeyResetsAt: &nov2, Usage: usage(f(0), f(0), f(0), f(0)), BYOK: usage(f(0), f(0), f(0), f(0)), Free: &freeRequests{Used: 0, Limit: 1000, Remaining: 1000}, Effective: &keyLimit}
	got, err := parseKey(fixture(t, "live_key_inference.json"), liveNow)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n got %s\nwant %s", err, dump(got), dump(want))
	}
	a, err := parseCredits(fixture(t, "live_credits_inference.json"))
	if err != nil || a.Credits != 245 || a.Usage != 237.537304708 || a.balance() != liveBalance() {
		t.Fatalf("%+v %v", a, err)
	}
	mgmt, err := parseKey(fixture(t, "live_key_management.json"), liveNow)
	if err != nil || !mgmt.Management || mgmt.Limit != nil || mgmt.Effective != nil || len(mgmt.Budgets) != 0 {
		t.Fatalf("%s %v", dump(mgmt), err)
	}
}

// The quota card for the live key, read end to end with the regular key.
func TestLiveQuotaCard(t *testing.T) {
	h := liveHarness(t)
	out, err := h.quota.FetchQuota(context.Background(), quotaRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	balance := liveBalance()
	wantGroups := []pluginapi.QuotaGroup{
		{DisplayName: "Limits", Buckets: []pluginapi.QuotaBucket{
			{Window: "Weekly key limit · $5.00 of $5.00 left", RemainingFraction: 1, ResetTime: "2026-11-02T00:00:00Z"},
			{Window: "Free-model requests today · 1000 of 1000 left", RemainingFraction: 1, ResetTime: "2026-11-02T00:00:00Z"},
		}},
	}
	if !reflect.DeepEqual(out.Groups, wantGroups) {
		t.Fatalf("groups\n got %+v\nwant %+v", out.Groups, wantGroups)
	}
	metrics := map[string]float64{}
	for _, m := range out.Summary {
		metrics[m.Key] = m.Value
	}
	wantMetrics := map[string]float64{"account_balance_usd": balance, "lifetime_spent_usd": liveUsage}
	if !reflect.DeepEqual(metrics, wantMetrics) {
		t.Fatalf("metrics %v", metrics)
	}
	if out.Subscription == nil || out.Subscription.TierName != "paid credits" {
		t.Fatalf("%+v", out.Subscription)
	}
	for _, seen := range h.srv.seen() {
		if seen != "/key Bearer "+exampleKey && seen != "/credits Bearer "+exampleKey {
			t.Fatalf("request %q", seen)
		}
	}
	if h.srv.creditCalls.Load() != 1 {
		t.Fatal("GET /credits must be read with the regular key")
	}
	r, _ := parseKey(fixture(t, "live_key_inference.json"), liveNow)
	r.Account, _ = parseCredits(fixture(t, "live_credits_inference.json"))
	golden(t, "quota_live_key_inference.json", normalizeQuota(r, liveNow))
}

func TestLiveGuard(t *testing.T) {
	h := liveHarness(t)
	if err := admitWith(h, credentials{apiKey: exampleKey}, "anthropic/claude-sonnet-4.5"); err != nil {
		t.Fatal(err)
	}
	if err := admitWith(h, credentials{apiKey: exampleKey}, "meta-llama/llama-3.3-70b-instruct:free"); err != nil {
		t.Fatal(err)
	}
	// A $5 reserve on the $5 weekly key refuses until Monday 00:00 UTC.
	e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey, reserve: 5}, "a/b"))
	if e.Code != "quota_exhausted" || e.RetryAfterSeconds != retrySeconds(utc("2026-11-02T00:00:00Z"), liveNow) || !strings.Contains(e.Message, "reserve") {
		t.Fatalf("%+v", e)
	}
	// The account balance is a guard layer, read with the regular key.
	if r := h.keys.peek("auth-1"); r == nil || r.Account == nil || r.Account.balance() != liveBalance() {
		t.Fatal("the guard reading lacks the account balance")
	}
}

func TestLiveManagementKeyIsRefused(t *testing.T) {
	h := newHarness(t, fixture(t, "live_key_management.json"))
	if e := rpcErr(t, admitWith(h, credentials{apiKey: exampleKey}, "a/b")); e.Code != "invalid_credentials" || !strings.Contains(e.Message, "management key") {
		t.Fatalf("%+v", e)
	}
	if _, err := h.quota.FetchQuota(context.Background(), quotaRequest("")); rpcErr(t, err).Code != "invalid_credentials" {
		t.Fatal("management key shown as a quota card")
	}
}
