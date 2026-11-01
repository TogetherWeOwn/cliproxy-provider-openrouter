package main

import (
	"testing"
	"time"
)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestCalendarResets(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(time.Time) time.Time
		in   string
		want string
	}{
		{"daily just before midnight", nextUTCDay, "2026-11-04T23:59:59.999Z", "2026-11-05T00:00:00Z"},
		{"daily at midnight", nextUTCDay, "2026-11-05T00:00:00Z", "2026-11-06T00:00:00Z"},
		{"daily month end", nextUTCDay, "2026-11-30T12:00:00Z", "2026-12-01T00:00:00Z"},
		{"weekly sunday last second", nextUTCMonday, "2026-11-08T23:59:59Z", "2026-11-09T00:00:00Z"},
		{"weekly monday midnight", nextUTCMonday, "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"weekly wednesday", nextUTCMonday, "2026-11-04T15:30:00Z", "2026-11-09T00:00:00Z"},
		{"monthly jan 31", nextUTCMonth, "2027-01-31T10:00:00Z", "2027-02-01T00:00:00Z"},
		{"monthly feb 28 2027", nextUTCMonth, "2027-02-28T23:00:00Z", "2027-03-01T00:00:00Z"},
		{"monthly feb 29 2028", nextUTCMonth, "2028-02-29T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"monthly dec 31", nextUTCMonth, "2026-12-31T23:59:59Z", "2027-01-01T00:00:00Z"},
		{"non-UTC input normalized", nextUTCDay, "2026-11-04T23:30:00-05:00", "2026-11-06T00:00:00Z"},
	} {
		if got := tc.fn(utc(tc.in)).Format(time.RFC3339); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestResolveReset(t *testing.T) {
	now := now0
	for _, tc := range []struct {
		name   string
		server *string
		w      window
		want   string
	}{
		{"valid server time wins", ptr("2026-11-04T17:30:00.000Z"), windowMonthly, "2026-11-04T17:30:00Z"},
		{"non-UTC offset normalized", ptr("2026-11-04T12:30:00-05:00"), windowDaily, "2026-11-04T17:30:00Z"},
		{"past server time falls back", ptr("2026-11-04T15:00:00Z"), windowDaily, "2026-11-05T00:00:00Z"},
		{"server time beyond 32 days falls back", ptr("2027-01-04T00:00:00Z"), windowWeekly, "2026-11-09T00:00:00Z"},
		{"garbage falls back", ptr("next tuesday"), windowMonthly, "2026-12-01T00:00:00Z"},
		{"absent uses calendar", nil, windowDaily, "2026-11-05T00:00:00Z"},
		{"lifetime never resets", nil, windowLifetime, ""},
		{"unknown never resets", nil, windowUnknown, ""},
		{"lifetime with garbage", ptr("soon"), windowLifetime, ""},
	} {
		got, ok := resolveReset(parseInstant(tc.server), tc.w, now)
		text := ""
		if ok {
			text = got.Format(time.RFC3339)
		}
		if text != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, text, tc.want)
		}
	}
}

func TestRetrySecondsClamp(t *testing.T) {
	if got := retrySeconds(now0.Add(-time.Hour), now0); got != 1 {
		t.Fatalf("past reset %d", got)
	}
	if got := retrySeconds(now0.Add(1500*time.Millisecond), now0); got != 2 {
		t.Fatalf("rounds up %d", got)
	}
	if got := retrySeconds(now0.Add(40*24*time.Hour), now0); got != 2764800 {
		t.Fatalf("clamp to 32 days %d", got)
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]window{"daily": windowDaily, "WEEKLY": windowWeekly, " monthly ": windowMonthly, "hourly": windowUnknown, "": windowUnknown} {
		if got := parseWindow(ptr(in), true); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	if parseWindow(nil, true) != windowLifetime || parseWindow(nil, false) != windowLifetime {
		t.Fatal("null or absent must be lifetime")
	}
}
