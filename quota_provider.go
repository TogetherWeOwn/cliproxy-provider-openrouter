package main

import (
	"context"
	"math"
	"strconv"
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

// usd formats dollars with at least 2 and at most 4 decimals, sign first:
// 25 -> $25.00, 9.996 -> $9.996, -0.02 -> -$0.02.
func usd(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	s := strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	if s == "0.00" {
		sign = ""
	}
	return sign + "$" + s
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

func keyGroup(r keyReading, c credentials, now time.Time) []pluginapi.QuotaBucket {
	if r.LimitMalformed {
		return []pluginapi.QuotaBucket{{Window: "Key limit: UNREADABLE (plugin refuses requests)", RemainingFraction: 0, Description: "OpenRouter reported a key limit that is not a number."}}
	}
	if r.Limit == nil {
		if c.allowUnlimited {
			// An unbounded key is never shown as headroom.
			return []pluginapi.QuotaBucket{{Window: "Key limit: NOT SET (UNBOUNDED spend)", RemainingFraction: 0, Description: "No credit limit is set; allow_unlimited_key admits requests and spend is limited only by account credits. Set a key limit at openrouter.ai/settings/keys."}}
		}
		return []pluginapi.QuotaBucket{{Window: "Key limit: NOT SET (plugin refuses requests)", RemainingFraction: 0, Description: "No credit limit is set on this key. Spend is unbounded, so the plugin refuses inference. Set a limit at openrouter.ai/settings/keys or set allow_unlimited_key."}}
	}
	b := pluginapi.QuotaBucket{Window: r.Reset.String() + " key limit", ResetTime: resetText(r.KeyResetsAt, r.Reset, now)}
	switch {
	case *r.Limit == 0:
		b.Description = "key limit is $0"
	case *r.Limit < 0:
		b.Description = "invalid key limit"
	case r.Remaining == nil:
		return nil
	default:
		b.RemainingFraction = fraction(*r.Remaining, *r.Limit)
		b.Description = usd(*r.Limit-*r.Remaining) + " of " + usd(*r.Limit) + " used; " + usd(*r.Remaining) + " left"
		if r.IncludeBYOK {
			b.Description += "; includes BYOK"
		}
	}
	return []pluginapi.QuotaBucket{b}
}

func budgetBucket(b budget, effective *budget, now time.Time) pluginapi.QuotaBucket {
	label, ok := scopeLabels[b.Scope]
	if !ok {
		label = "unknown"
	}
	out := pluginapi.QuotaBucket{Window: b.Interval.String() + " " + label + " budget", ResetTime: resetText(b.ResetsAt, b.Interval, now)}
	rem := b.remaining()
	if b.Limit == nil || *b.Limit <= 0 || rem == nil {
		out.Description = "invalid budget"
		return out
	}
	out.RemainingFraction = fraction(*rem, *b.Limit)
	spend := *b.Limit - *rem
	if b.Spend != nil {
		spend = *b.Spend
	}
	out.Description = usd(spend) + " of " + usd(*b.Limit)
	if b.same(effective) {
		out.Description += "; binding"
	}
	return out
}

func money(key, label string, v *float64) []pluginapi.QuotaMetric {
	if v == nil {
		return nil
	}
	return []pluginapi.QuotaMetric{{Key: key, Label: label, Value: *v, Format: "currency", Currency: "USD"}}
}

// normalizeQuota builds the card: key limit, budgets, free-model requests
// and account credits as buckets; spend counters as summary metrics. Usage
// windows are counters, not caps, so they have no remaining fraction.
func normalizeQuota(r keyReading, c credentials, now time.Time) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "OpenRouter", TierName: "paid credits"}}
	switch {
	case r.LimitMalformed:
		out.Subscription.TierName = "KEY LIMIT UNREADABLE"
	case r.Limit == nil:
		out.Subscription.TierName = "NO KEY LIMIT"
	case r.FreeTier:
		out.Subscription.TierName = "free tier"
	}
	if buckets := keyGroup(r, c, now); len(buckets) > 0 {
		out.Groups = append(out.Groups, pluginapi.QuotaGroup{DisplayName: "Key credit limit", Buckets: buckets})
	}
	if len(r.Budgets) > 0 {
		g := pluginapi.QuotaGroup{DisplayName: "Budgets"}
		for _, b := range r.Budgets {
			g.Buckets = append(g.Buckets, budgetBucket(b, r.Effective, now))
		}
		out.Groups = append(out.Groups, g)
	}
	if r.Free != nil && r.Free.Limit > 0 {
		out.Groups = append(out.Groups, pluginapi.QuotaGroup{DisplayName: "Free models (:free)", Buckets: []pluginapi.QuotaBucket{{
			Window: "daily free-model requests", RemainingFraction: fraction(float64(r.Free.Remaining), float64(r.Free.Limit)), ResetTime: nextUTCDay(now).Format(time.RFC3339),
			Description: strconv.FormatInt(r.Free.Used, 10) + "/" + strconv.FormatInt(r.Free.Limit, 10) + " requests today (account-wide; 20/min not reported)",
		}}})
	}
	if r.Account != nil {
		out.Groups = append(out.Groups, pluginapi.QuotaGroup{DisplayName: "Account credits", Buckets: []pluginapi.QuotaBucket{{
			Window: "account credit balance (no reset; top-up)", RemainingFraction: fraction(r.Account.balance(), r.Account.Credits),
			Description: usd(r.Account.balance()) + " of " + usd(r.Account.Credits) + " purchased",
		}}})
	}
	m := &out.Summary
	*m = append(*m, money("key_limit_usd", "Key limit", r.Limit)...)
	*m = append(*m, money("key_remaining_usd", "Key limit remaining", r.Remaining)...)
	periods := [4][2]string{{"total", "all time"}, {"day", "today (UTC)"}, {"week", "this week (Mon-Sun UTC)"}, {"month", "this month (UTC)"}}
	for _, i := range []int{usageDaily, usageWeekly, usageMonthly, usageTotal} {
		*m = append(*m, money("key_spend_"+periods[i][0]+"_usd", "Key spend "+periods[i][1], r.Usage[i])...)
	}
	if r.BYOK[usageTotal] != nil && *r.BYOK[usageTotal] > 0 {
		for _, i := range []int{usageDaily, usageWeekly, usageMonthly, usageTotal} {
			*m = append(*m, money("byok_spend_"+periods[i][0]+"_usd", "BYOK spend "+periods[i][1], r.BYOK[i])...)
		}
	}
	if r.Account != nil {
		balance := r.Account.balance()
		*m = append(*m, money("account_balance_usd", "Account credit balance", &balance)...)
	}
	if c.reserve > 0 {
		reserve := c.reserve
		*m = append(*m, money("spend_reserve_usd", "Plugin spend reserve", &reserve)...)
	}
	if r.Free != nil {
		*m = append(*m, pluginapi.QuotaMetric{Key: "free_requests_used", Label: "Free-model requests today", Value: float64(r.Free.Used), Unit: "requests", Format: "number"})
	}
	if r.ExpiresAt != nil {
		days := math.Round(r.ExpiresAt.Sub(now).Hours()/24*10) / 10
		*m = append(*m, pluginapi.QuotaMetric{Key: "key_expires_in_days", Label: "Key expires in (days)", Value: days, Unit: "days", Format: "number"})
	}
	return out
}
