package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
)

func usage(total, day, week, month *float64) [4]*float64 {
	return [4]*float64{total, day, week, month}
}

func TestParseKeyFixtures(t *testing.T) {
	f := ptr[float64]
	dec1 := utc("2026-12-01T00:00:00Z")
	nov5 := utc("2026-11-05T00:00:00Z")
	keyLimitBudget := budget{Scope: "key_limit", Limit: f(25), Spend: f(14.5), Remaining: f(10.5), Interval: windowMonthly, ResetsAt: &dec1}
	member := budget{Scope: "member", GuardrailID: ptr("guardrail-example-1"), Limit: f(10), Spend: f(7), Remaining: f(3), Interval: windowDaily, ResetsAt: &nov5}
	zeroByok := usage(f(0), f(0), f(0), f(0))
	for _, tc := range []struct {
		file string
		want keyReading
	}{
		{"key_monthly_limit.json", keyReading{Limit: f(25), Remaining: f(10.5), Reset: windowMonthly, KeyResetsAt: &dec1, Usage: usage(f(40.25), f(1.5), f(4), f(14.5)), BYOK: zeroByok, Free: &freeRequests{3, 1000, 997}, Effective: &keyLimitBudget}},
		{"key_daily_limit_no_budgets.json", keyReading{Limit: f(2), Remaining: f(1.25), Reset: windowDaily, IncludeBYOK: true, Usage: usage(f(9), f(0.75), f(3), f(9)), BYOK: usage(f(1.5), f(0.25), f(0.5), f(1.5))}},
		{"key_weekly_limit.json", keyReading{Limit: f(10), Remaining: f(0.004), Reset: windowWeekly, Usage: usage(f(30), f(2), f(9.996), f(30))}},
		{"key_lifetime_limit.json", keyReading{Limit: f(100), Remaining: f(60), Reset: windowLifetime, Usage: usage(f(40), f(0), f(0), f(5))}},
		{"key_no_limit.json", keyReading{Reset: windowLifetime, Usage: usage(f(12.5), f(0.5), f(2), f(7)), BYOK: usage(f(0), nil, nil, nil)}},
		{"key_exhausted_negative.json", keyReading{Limit: f(5), Remaining: f(-0.02), Reset: windowMonthly, Usage: usage(f(5.02), f(0.4), f(1.2), f(5.02))}},
		{"key_zero_limit.json", keyReading{Limit: f(0), Remaining: f(0), Reset: windowDaily, Usage: usage(f(0), f(0), f(0), f(0))}},
		{"key_unknown_reset.json", keyReading{Limit: f(3), Remaining: f(2), Reset: windowUnknown, Usage: usage(f(1), f(1), f(1), f(1))}},
		{"key_budgets_mixed.json", keyReading{Reset: windowLifetime, Usage: usage(f(80), f(4), f(20), f(60)), Budgets: []budget{
			member,
			{Scope: "workspace", BudgetID: ptr("budget-example-1"), Limit: f(500), Spend: f(120), Remaining: f(380), Interval: windowLifetime},
			{Scope: "workspace", BudgetID: ptr("budget-example-2"), Limit: f(200), Spend: f(60), Interval: windowMonthly, ResetsAt: &dec1, IncludeBYOK: true},
			{Scope: "unknown", Limit: f(0), Spend: f(0), Remaining: f(0), Interval: windowUnknown},
		}, Effective: &member}},
		{"key_free_tier.json", keyReading{Limit: f(1), Remaining: f(1), Reset: windowMonthly, FreeTier: true, Usage: usage(f(0), f(0), f(0), f(0)), Free: &freeRequests{50, 50, 0}}},
		{"key_free_paid.json", keyReading{Limit: f(20), Remaining: f(19), Reset: windowMonthly, Usage: usage(f(1), f(0), f(1), f(1)), Free: &freeRequests{3, 1000, 997}}},
		{"key_management.json", keyReading{Reset: windowLifetime, Management: true, Usage: usage(f(0), nil, nil, nil)}},
		{"key_provisioning_legacy.json", keyReading{Reset: windowLifetime, Management: true, Usage: usage(f(0), nil, nil, nil)}},
		{"key_expiring.json", keyReading{Limit: f(50), Remaining: f(50), Reset: windowMonthly, Usage: usage(f(0), f(0), f(0), f(0)), ExpiresAt: ptr(utc("2026-11-07T03:30:00Z"))}},
		// Wrong types read as absent; a wrong-typed limit_reset is unknown, not lifetime.
		{"key_wrong_types.json", keyReading{Reset: windowUnknown, LimitMalformed: true}},
		{"key_extra_unknown_fields.json", keyReading{Limit: f(8), Remaining: f(6), Reset: windowWeekly, Usage: usage(f(2), f(1), f(2), f(2))}},
	} {
		got, err := parseKey(fixture(t, tc.file), now0)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		tc.want.FetchedAt = now0
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %s\nwant %s", tc.file, dump(got), dump(tc.want))
		}
	}
}

func dump(r keyReading) string {
	var b strings.Builder
	v := reflect.ValueOf(r)
	for i := 0; i < v.NumField(); i++ {
		b.WriteString(v.Type().Field(i).Name + "=")
		b.WriteString(deref(v.Field(i)) + " ")
	}
	return b.String()
}

func deref(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return "nil"
		}
		return "&" + deref(v.Elem())
	case reflect.Array, reflect.Slice:
		parts := []string{}
		for i := 0; i < v.Len(); i++ {
			parts = append(parts, deref(v.Index(i)))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			return t.Format(time.RFC3339)
		}
		parts := []string{}
		for i := 0; i < v.NumField(); i++ {
			parts = append(parts, v.Type().Field(i).Name+":"+deref(v.Field(i)))
		}
		return "{" + strings.Join(parts, " ") + "}"
	}
	return strings.TrimSpace(strings.ReplaceAll(reflect.ValueOf(v.Interface()).String(), "\n", ""))
}

func TestParseKeyRejectsInvalidBodies(t *testing.T) {
	for _, file := range []string{"key_not_json.txt", "key_no_data.json"} {
		_, err := parseKey(fixture(t, file), now0)
		if e := rpcErr(t, err); e.Code != "invalid_quota" || e.HTTPStatus != 502 {
			t.Fatalf("%s: %+v", file, e)
		}
	}
	if _, err := parseKey([]byte(`{"data":null}`), now0); err == nil {
		t.Fatal("null data accepted")
	}
}

func TestSanitizeEnum(t *testing.T) {
	for in, want := range map[string]string{"member": "member", "WORKSPACE": "workspace", "Team-Cap!": "unknown", "workspace_default_member_and_more_text": "unknown", "<script>": "unknown"} {
		if got := sanitizeEnum(in, budgetScopes); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestParseCredits(t *testing.T) {
	a, err := parseCredits(fixture(t, "credits_ok.json"))
	if err != nil || a.Credits != 100.5 || a.Usage != 25.75 || a.balance() != 74.75 {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err = parseCredits(fixture(t, "credits_zero.json")); err != nil || a.Credits != 0 {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err = parseCredits(fixture(t, "credits_403.json")); err == nil {
		t.Fatal("error body accepted as credits")
	}
}

func TestFetchKeyErrors(t *testing.T) {
	ctx := context.Background()
	client := kit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	s := newKeyServer(t, fixture(t, "key_401.json"))
	s.set(fixture(t, "key_401.json"), 401)
	_, err := fetchKey(ctx, client, s.URL, exampleKey, now0)
	if e := rpcErr(t, err); e.Code != "invalid_credentials" || e.HTTPStatus != 401 || strings.Contains(e.Message, exampleKey) {
		t.Fatalf("401: %+v", e)
	}
	s.set([]byte(`{"error":{"code":500}}`), 500)
	if e := rpcErr(t, func() error { _, err := fetchKey(ctx, client, s.URL, exampleKey, now0); return err }()); e.Code != "quota_unavailable" {
		t.Fatalf("500: %+v", e)
	}
	s.set(fixture(t, "key_not_json.txt"), 200)
	if e := rpcErr(t, func() error { _, err := fetchKey(ctx, client, s.URL, exampleKey, now0); return err }()); e.Code != "invalid_quota" {
		t.Fatalf("not json: %+v", e)
	}
	// key_oversize: one byte over the 1 MiB quota body cap.
	s.set([]byte(`{"data":{"limit":1,"pad":"`+strings.Repeat("x", 1<<20)+`"}}`), 200)
	if e := rpcErr(t, func() error { _, err := fetchKey(ctx, client, s.URL, exampleKey, now0); return err }()); e.Code != "invalid_quota" || strings.Contains(e.Message, exampleKey) {
		t.Fatalf("oversize: %+v", e)
	}
	for _, seen := range s.seen() {
		if seen != "/key Bearer "+exampleKey {
			t.Fatalf("request %q", seen)
		}
	}
}
