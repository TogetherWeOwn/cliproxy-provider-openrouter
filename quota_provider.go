package main

import (
	"context"
	"fmt"
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
		return normalizeQuota(reading, keys.now()), nil
	})}
}

// Dollar amounts in bar labels use the shared kit formatter (exactly 2
// decimals). Bar labels are raw strings (the panel formats only chips), so
// labels fix 2 decimals instead of inheriting the panel's chip formatting.
func usd2(v float64) string { return kit.USD(v) }

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

func keyLimitName(reset window) string {
	switch reset {
	case windowWeekly:
		return "Weekly key limit"
	case windowDaily:
		return "Daily key limit"
	case windowMonthly:
		return "Monthly key limit"
	default:
		return "Key limit"
	}
}

func keyGroup(r keyReading, now time.Time) []pluginapi.QuotaBucket {
	if r.LimitMalformed {
		return []pluginapi.QuotaBucket{kit.NewQuotaBucket("Key limit: UNREADABLE (plugin refuses requests)", 0, "")}
	}
	if r.Limit == nil {
		// A key without a limit has no remaining fraction to show: a
		// zero-fraction bucket reads as exhausted. Omit the bucket; the
		// card keeps the NO KEY LIMIT tier and the balance chips. Whether
		// requests are admitted is the guard's job
		// (allow_unlimited_key), not the card's.
		return nil
	}
	name := keyLimitName(r.Reset)
	reset := resetText(r.KeyResetsAt, r.Reset, now)
	switch {
	case *r.Limit == 0:
		return []pluginapi.QuotaBucket{kit.NewQuotaBucket(name+" · limit is $0", 0, reset)}
	case *r.Limit < 0:
		return []pluginapi.QuotaBucket{kit.NewQuotaBucket(name+" · invalid key limit", 0, reset)}
	case r.Remaining == nil:
		return nil
	default:
		label := fmt.Sprintf("%s · %s of %s left", name, usd2(*r.Remaining), usd2(*r.Limit))
		if r.IncludeBYOK {
			label += "; includes BYOK"
		}
		return []pluginapi.QuotaBucket{kit.NewQuotaBucket(label, fraction(*r.Remaining, *r.Limit), reset)}
	}
}

func budgetBucket(b budget, effective *budget, now time.Time) pluginapi.QuotaBucket {
	label, ok := scopeLabels[b.Scope]
	if !ok {
		label = "unknown"
	}
	name := b.Interval.String() + " " + label + " budget"
	if name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	rem := b.remaining()
	reset := resetText(b.ResetsAt, b.Interval, now)
	if b.Limit == nil || *b.Limit <= 0 || rem == nil {
		return kit.NewQuotaBucket(name+" · invalid budget", 0, reset)
	}
	spend := *b.Limit - *rem
	if b.Spend != nil {
		spend = *b.Spend
	}
	amounts := fmt.Sprintf("%s of %s spent", usd2(spend), usd2(*b.Limit))
	if b.same(effective) {
		amounts += "; binding"
	}
	return kit.NewQuotaBucket(name+" · "+amounts, fraction(*rem, *b.Limit), reset)
}

func money(key, label string, v *float64) []pluginapi.QuotaMetric {
	if v == nil {
		return nil
	}
	return []pluginapi.QuotaMetric{{Key: key, Label: label, Value: *v, Format: "currency", Currency: "USD"}}
}

// normalizeQuota builds the card: one short plan chip, at most two summary
// chips (balance and lifetime spend), and a Limits group with one bar per
// real rate window (key credit limit, free-model requests). Budgets get their
// own group only when non-key-limit budgets exist. The account credit balance
// is a shared pool with no reset, so it is a plain chip, never a bar: a
// balance-over-lifetime-purchases fraction only shrinks toward zero.
func normalizeQuota(r keyReading, now time.Time) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "OpenRouter", TierName: "paid credits"}}
	switch {
	case r.LimitMalformed:
		out.Subscription.TierName = "KEY LIMIT UNREADABLE"
	case r.Limit == nil:
		out.Subscription.TierName = "NO KEY LIMIT"
	case r.FreeTier:
		out.Subscription.TierName = "free tier"
	}
	limits := pluginapi.QuotaGroup{DisplayName: "Limits"}
	if buckets := keyGroup(r, now); len(buckets) > 0 {
		limits.Buckets = append(limits.Buckets, buckets...)
	}
	if r.Free != nil && r.Free.Limit > 0 {
		limits.Buckets = append(limits.Buckets, kit.NewQuotaBucket(
			fmt.Sprintf("Free-model requests today · %d of %d left", r.Free.Remaining, r.Free.Limit),
			fraction(float64(r.Free.Remaining), float64(r.Free.Limit)),
			nextUTCDay(now).Format(time.RFC3339)))
	}
	if len(limits.Buckets) > 0 {
		out.Groups = append(out.Groups, limits)
	}
	if len(r.Budgets) > 0 {
		g := pluginapi.QuotaGroup{DisplayName: "Budgets"}
		for _, b := range r.Budgets {
			g.Buckets = append(g.Buckets, budgetBucket(b, r.Effective, now))
		}
		out.Groups = append(out.Groups, g)
	}
	if r.Account != nil {
		balance := r.Account.balance()
		out.Summary = append(out.Summary, money("account_balance_usd", "Balance", &balance)...)
		out.Summary = append(out.Summary, money("lifetime_spent_usd", "Lifetime spent", &r.Account.Usage)...)
	}
	if r.ExpiresAt != nil {
		days := math.Round(r.ExpiresAt.Sub(now).Hours()/24*10) / 10
		if days >= 0 && days <= 7 {
			out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "key_expires_in_days", Label: "Key expires in (days)", Value: days, Unit: "days", Format: "number"})
		}
	}
	return out
}
