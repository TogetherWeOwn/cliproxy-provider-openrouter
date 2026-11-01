package main

import (
	"math"
	"strings"
	"time"
)

// window is the reset cadence of a key limit or budget. OpenRouter windows
// are UTC calendar windows, not rolling ones: a daily window resets at 00:00
// UTC, a weekly one on Monday 00:00 UTC and a monthly one on the first of the
// month 00:00 UTC.
type window int

const (
	windowUnknown window = iota
	windowDaily
	windowWeekly
	windowMonthly
	windowLifetime
)

func (w window) String() string {
	switch w {
	case windowDaily:
		return "daily"
	case windowWeekly:
		return "weekly"
	case windowMonthly:
		return "monthly"
	case windowLifetime:
		return "lifetime"
	}
	return "unknown-reset"
}

// parseWindow maps a documented interval to a window. A JSON null or absent
// value means the limit never resets; anything else is unknown.
func parseWindow(raw *string, present bool) window {
	if !present || raw == nil {
		return windowLifetime
	}
	switch strings.ToLower(strings.TrimSpace(*raw)) {
	case "daily":
		return windowDaily
	case "weekly":
		return windowWeekly
	case "monthly":
		return windowMonthly
	}
	return windowUnknown
}

const (
	// maxRetry bounds every reset-derived wait: no window is longer than a month.
	maxRetry = 32 * 24 * time.Hour
	// recheckSeconds is the wait for exhaustion without a known reset: the
	// owner may raise the limit or top up at any time.
	recheckSeconds = 3600
)

func nextUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextUTCMonday(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	idx := (int(d.Weekday()) + 6) % 7 // Monday = 0
	return d.AddDate(0, 0, 7-idx)
}

func nextUTCMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// parseInstant accepts RFC 3339 timestamps (with or without fractions) and
// returns them in UTC. Anything else is treated as absent.
func parseInstant(raw *string) *time.Time {
	if raw == nil {
		return nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" || len(s) > 64 {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// resolveReset prefers a plausible server-reported reset instant and falls
// back to the UTC calendar rule for the window. ok is false when the window
// never resets or its reset is unknown.
func resolveReset(server *time.Time, interval window, now time.Time) (time.Time, bool) {
	if server != nil && server.After(now) && !server.After(now.Add(maxRetry)) {
		return server.UTC(), true
	}
	switch interval {
	case windowDaily:
		return nextUTCDay(now), true
	case windowWeekly:
		return nextUTCMonday(now), true
	case windowMonthly:
		return nextUTCMonth(now), true
	}
	return time.Time{}, false
}

// retrySeconds converts a reset instant into a host retry hint.
func retrySeconds(reset, now time.Time) int {
	s := math.Ceil(reset.Sub(now).Seconds())
	return int(math.Min(math.Max(s, 1), maxRetry.Seconds()))
}
