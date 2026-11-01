package main

import (
	"context"
	"math"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaProvider is the display-only quota card. The host never routes on it;
// enforcement is the guard's job. It shares keyState with the guard, and the
// kit cache (60 s, single-flight, errors never cached) sits on top.
type quotaProvider struct{ *kit.CachedQuota }

func newQuotaProvider(keys *keyState, g *guard) *quotaProvider {
	return &quotaProvider{kit.NewCachedQuota(provider, "OpenRouter", func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		g.observeHost(req.Host)
		c, err := credentialsFrom(req.StorageJSON, req.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		id := req.AuthID
		if id == "" {
			id = req.AuthIndex
		}
		// The kit already attached the auth or host proxy to ctx.
		reading, err := keys.get(ctx, id, c, 0)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		if reading.Management {
			return pluginapi.QuotaFetchResponse{}, managementKeyError()
		}
		return normalizeQuota(reading, c, keys.now()), nil
	})}
}

func fraction(remaining, limit float64) float64 {
	if !(limit > 0) {
		return 0
	}
	return math.Min(math.Max(remaining/limit, 0), 1)
}

func resetText(server *time.Time, w window, now time.Time) string {
	if reset, ok := resolveReset(server, w, now); ok {
		return reset.UTC().Format(time.RFC3339)
	}
	return ""
}

var scopeLabels = map[string]string{"member": "member guardrail", "workspace_default_member": "member guardrail", "api_key": "key guardrail", "workspace_default_key": "key guardrail", "workspace": "workspace"}

// keyLimitWindow is the short row label for the key-limit bar.
func keyLimitWindow(w window) string {
	switch w {
	case windowDaily:
		return "Daily key limit"
	case windowWeekly:
		return "Weekly key limit"
	case windowMonthly:
		return "Monthly key limit"
	case windowLifetime:
		return "Lifetime key limit"
	default:
		return "Key limit"
	}
}

// keyBar renders the key-limit bar: a 0% warning bar when the limit is
// malformed, zero, negative, or its remaining is unknown; nil when the key
// has no limit (a zero-fraction bucket would read as exhausted, so the
// no-limit hunk omits the bucket and the card keeps the NO KEY LIMIT tier).
// Whether requests are admitted is the guard's job, not the card's.
func keyBar(r keyReading, now time.Time) *pluginapi.QuotaBucket {
	if r.LimitMalformed {
		return &pluginapi.QuotaBucket{Window: "Key limit unreadable"}
	}
	if r.Limit == nil {
		return nil
	}
	b := &pluginapi.QuotaBucket{Window: keyLimitWindow(r.Reset), ResetTime: resetText(r.KeyResetsAt, r.Reset, now)}
	if *r.Limit <= 0 || r.Remaining == nil {
		return b
	}
	b.RemainingFraction = fraction(*r.Remaining, *r.Limit)
	return b
}

// budgetWindow is the short row label for a non-key_limit budget bar.
func budgetWindow(b budget) string {
	label, ok := scopeLabels[b.Scope]
	if !ok || label == "" {
		label = "unknown"
	}
	var interval string
	switch b.Interval {
	case windowDaily:
		interval = "Daily"
	case windowWeekly:
		interval = "Weekly"
	case windowMonthly:
		interval = "Monthly"
	case windowLifetime:
		interval = "Lifetime"
	}
	return strings.TrimSpace(interval + " " + label + " budget")
}

// budgetBar renders one non-key_limit budget bar; an invalid budget (no
// positive limit or no computable remaining) stays a 0% warning bar.
func budgetBar(b budget, now time.Time) pluginapi.QuotaBucket {
	out := pluginapi.QuotaBucket{Window: budgetWindow(b), ResetTime: resetText(b.ResetsAt, b.Interval, now)}
	if rem := b.remaining(); b.Limit != nil && *b.Limit > 0 && rem != nil {
		out.RemainingFraction = fraction(*rem, *b.Limit)
	}
	return out
}

// expiryChip renders the key expiry as a plan-row chip. Panel chips are
// label-plus-number only, so the chip shows days remaining ("Key expires in")
// — an absolute date has no numeric value to render. A future expiry always
// shows at least 1 day so a key expiring later today does not read as
// expired; an elapsed expiry shows 0, keeping the indicator visible for
// cached readings that cross their expiry.
func expiryChip(expires time.Time, now time.Time) pluginapi.QuotaMetric {
	days := math.Round(expires.Sub(now).Hours() / 24)
	if expires.After(now) && days < 1 {
		days = 1
	}
	if days < 0 {
		days = 0
	}
	return pluginapi.QuotaMetric{Key: "key_expires_in_days", Label: "Key expires in", Value: days, Unit: "days", Format: "number"}
}

// normalizeQuota builds the single-group card: the key-limit bar, the
// free-model daily bar, and one bar per non-key_limit budget when present.
// Summary chips are the account balance and lifetime spend (USD currency,
// formatted panel-side with 2 decimals) plus the expiry chip when the key
// has an expiry. Usage counters, BYOK splits, the spend reserve, and
// purchased-credit totals stay off the card.
func normalizeQuota(r keyReading, _ credentials, now time.Time) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "OpenRouter", TierName: "paid credits"}}
	switch {
	case r.LimitMalformed:
		out.Subscription.TierName = "KEY LIMIT UNREADABLE"
	case r.Limit == nil:
		out.Subscription.TierName = "NO KEY LIMIT"
	case r.FreeTier:
		out.Subscription.TierName = "free tier"
	}
	var bars []pluginapi.QuotaBucket
	if b := keyBar(r, now); b != nil {
		bars = append(bars, *b)
	}
	if r.Free != nil && r.Free.Limit > 0 {
		bars = append(bars, pluginapi.QuotaBucket{
			Window: "Free-model requests today", RemainingFraction: fraction(float64(r.Free.Remaining), float64(r.Free.Limit)), ResetTime: nextUTCDay(now).Format(time.RFC3339),
		})
	}
	for _, b := range r.Budgets {
		bars = append(bars, budgetBar(b, now))
	}
	if len(bars) > 0 {
		out.Groups = append(out.Groups, pluginapi.QuotaGroup{DisplayName: "OpenRouter", Buckets: bars})
	}
	if r.Account != nil {
		out.Summary = append(out.Summary,
			pluginapi.QuotaMetric{Key: "account_balance_usd", Label: "Balance", Value: r.Account.balance(), Format: "currency", Currency: "USD"},
			pluginapi.QuotaMetric{Key: "lifetime_spent_usd", Label: "Lifetime spent", Value: r.Account.Usage, Format: "currency", Currency: "USD"},
		)
	}
	if r.ExpiresAt != nil {
		out.Summary = append(out.Summary, expiryChip(*r.ExpiresAt, now))
	}
	return out
}
