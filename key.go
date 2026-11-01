package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// httpDoer is the plugin's own bounded GET client (kit.QuotaHTTPClient). It
// is used for GET /key and GET /credits only, never for inference.
type httpDoer interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

const maxBudgets = 16

// keyReading is one parsed GET /key observation, plus GET /credits when that
// read succeeded (Account is nil otherwise). Money fields are nil when
// OpenRouter did not report a finite number; the parser never invents a value.
// The /key usage counters lag real spend: a reading is never real time.
type keyReading struct {
	FetchedAt        time.Time
	Limit, Remaining *float64 // nil Limit: no key limit
	// LimitMalformed: "limit" is present but neither null nor a finite
	// number. The guard fails closed instead of treating it as no limit.
	LimitMalformed bool
	Reset          window
	KeyResetsAt    *time.Time // budgets[scope=key_limit].resets_at
	IncludeBYOK    bool
	Usage          [4]*float64 // total, daily, weekly, monthly
	BYOK           [4]*float64
	FreeTier       bool
	Management     bool
	ExpiresAt      *time.Time
	Free           *freeRequests
	Budgets        []budget // excludes scope=key_limit
	Effective      *budget
	Account        *account
}

const (
	usageTotal = iota
	usageDaily
	usageWeekly
	usageMonthly
)

type freeRequests struct{ Used, Limit, Remaining int64 }

type account struct{ Credits, Usage float64 }

func (a account) balance() float64 { return a.Credits - a.Usage }

// budget is one sanitized budgets[] entry. IDs are kept only to match the
// effective budget and are never displayed.
type budget struct {
	Scope                   string
	GuardrailID, BudgetID   *string
	Limit, Spend, Remaining *float64
	Interval                window
	ResetsAt                *time.Time
	IncludeBYOK             bool
}

// remaining is remaining_usd, else limit_usd - spend_usd when both exist.
func (b budget) remaining() *float64 {
	if b.Remaining != nil {
		return b.Remaining
	}
	if b.Limit != nil && b.Spend != nil {
		v := *b.Limit - *b.Spend
		return &v
	}
	return nil
}

func (b budget) same(o *budget) bool {
	return o != nil && b.Scope == o.Scope && eqStr(b.GuardrailID, o.GuardrailID) && eqStr(b.BudgetID, o.BudgetID)
}

func eqStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

var budgetScopes = map[string]bool{"workspace_default_member": true, "workspace_default_key": true, "member": true, "api_key": true, "workspace": true, "key_limit": true}

// sanitizeEnum lowercases and keeps [a-z_]; the result is returned only when
// it is a documented value of at most 24 bytes, else "unknown". Raw upstream
// text is never stored.
func sanitizeEnum(raw string, allowed map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() > 24 {
			return "unknown"
		}
	}
	if s := b.String(); allowed[s] {
		return s
	}
	return "unknown"
}

// object is a tolerant JSON object view: a field of the wrong type reads as
// absent instead of failing the whole parse.
type object map[string]json.RawMessage

func decodeObject(raw []byte) (object, bool) {
	var o object
	if json.Unmarshal(raw, &o) != nil || o == nil {
		return nil, false
	}
	return o, true
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func (o object) num(name string) *float64 {
	raw, ok := o[name]
	if !ok {
		return nil
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || !(s[0] == '-' || (s[0] >= '0' && s[0] <= '9')) {
		return nil
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func (o object) integer(name string) *int64 {
	raw, ok := o[name]
	if !ok {
		return nil
	}
	var v int64
	if json.Unmarshal(raw, &v) != nil || isNull(raw) {
		return nil
	}
	return &v
}

func (o object) boolean(name string) bool {
	var v bool
	raw, ok := o[name]
	return ok && json.Unmarshal(raw, &v) == nil && v
}

// str returns the string value, and present reports whether the field exists
// as a string or null (null yields nil). A wrong type is absent.
func (o object) str(name string) (value *string, present bool) {
	raw, ok := o[name]
	if !ok {
		return nil, false
	}
	if isNull(raw) {
		return nil, true
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return &v, true
}

func (o object) child(name string) (object, bool) {
	raw, ok := o[name]
	if !ok || isNull(raw) {
		return nil, false
	}
	return decodeObject(raw)
}

func idField(o object, name string) *string {
	v, _ := o.str(name)
	if v == nil {
		return nil
	}
	s := *v
	if len(s) > 64 {
		s = s[:64]
	}
	return &s
}

func parseBudget(o object) budget {
	scope, _ := o.str("scope")
	b := budget{Scope: "unknown", GuardrailID: idField(o, "guardrail_id"), BudgetID: idField(o, "budget_id"), Limit: o.num("limit_usd"), Spend: o.num("spend_usd"), Remaining: o.num("remaining_usd"), IncludeBYOK: o.boolean("include_byok")}
	if scope != nil {
		b.Scope = sanitizeEnum(*scope, budgetScopes)
	}
	interval, present := o.str("reset_interval")
	if _, exists := o["reset_interval"]; exists && !present {
		b.Interval = windowUnknown // wrong JSON type
	} else {
		b.Interval = parseWindow(interval, present)
	}
	at, _ := o.str("resets_at")
	b.ResetsAt = parseInstant(at)
	return b
}

func invalidQuota() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_quota", Message: "invalid_quota: OpenRouter returned an invalid key status", HTTPStatus: http.StatusBadGateway}
}
func quotaUnavailable() *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_unavailable", Message: "quota_unavailable: OpenRouter key status could not be read", HTTPStatus: http.StatusBadGateway, Retryable: true}
}
func invalidCredentials(message string) *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_credentials", Message: "invalid_credentials: " + message, HTTPStatus: http.StatusUnauthorized}
}
func managementKeyError() *pluginabi.Error {
	return invalidCredentials("api_key is a management key and cannot run inference")
}

// parseKey parses a GET /key body. Only a non-object body or a missing data
// object fails; individual malformed fields read as absent.
func parseKey(raw []byte, fetchedAt time.Time) (keyReading, error) {
	top, ok := decodeObject(raw)
	if !ok {
		return keyReading{}, invalidQuota()
	}
	d, ok := top.child("data")
	if !ok {
		return keyReading{}, invalidQuota()
	}
	r := keyReading{FetchedAt: fetchedAt, Limit: d.num("limit"), Remaining: d.num("limit_remaining"), IncludeBYOK: d.boolean("include_byok_in_limit"), FreeTier: d.boolean("is_free_tier")}
	if raw, ok := d["limit"]; ok && !isNull(raw) && r.Limit == nil {
		r.LimitMalformed = true
	}
	r.Management = d.boolean("is_management_key") || d.boolean("is_provisioning_key")
	reset, present := d.str("limit_reset")
	if _, exists := d["limit_reset"]; exists && !present {
		r.Reset = windowUnknown
	} else {
		r.Reset = parseWindow(reset, present)
	}
	for i, name := range []string{"usage", "usage_daily", "usage_weekly", "usage_monthly"} {
		r.Usage[i] = d.num(name)
		r.BYOK[i] = d.num("byok_" + name)
	}
	expires, _ := d.str("expires_at")
	r.ExpiresAt = parseInstant(expires)
	if f, ok := d.child("free_model_daily_requests"); ok {
		used, limit, remaining := f.integer("used"), f.integer("limit"), f.integer("remaining")
		if used != nil && limit != nil && remaining != nil {
			r.Free = &freeRequests{Used: *used, Limit: *limit, Remaining: *remaining}
		}
	}
	var entries []json.RawMessage
	if raw, ok := d["budgets"]; ok && json.Unmarshal(raw, &entries) == nil {
		for _, entry := range entries {
			o, ok := decodeObject(entry)
			if !ok {
				continue
			}
			b := parseBudget(o)
			if b.Scope == "key_limit" {
				if r.KeyResetsAt == nil {
					r.KeyResetsAt = b.ResetsAt
				}
				continue
			}
			if len(r.Budgets) < maxBudgets {
				r.Budgets = append(r.Budgets, b)
			}
		}
	}
	if e, ok := d.child("effective_budget"); ok {
		b := parseBudget(e)
		r.Effective = &b
	}
	return r, nil
}

// parseCredits parses GET /credits; both totals must be finite numbers.
func parseCredits(raw []byte) (*account, error) {
	top, ok := decodeObject(raw)
	if !ok {
		return nil, invalidQuota()
	}
	d, ok := top.child("data")
	if !ok {
		return nil, invalidQuota()
	}
	credits, usage := d.num("total_credits"), d.num("total_usage")
	if credits == nil || usage == nil {
		return nil, invalidQuota()
	}
	return &account{Credits: *credits, Usage: *usage}, nil
}

// fetchError maps a kit quota-client error to a public error. Keys, URLs and
// bodies never reach the message.
func fetchError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var e *pluginabi.Error
	if errors.As(err, &e) && e != nil {
		switch {
		case e.HTTPStatus == http.StatusUnauthorized:
			return invalidCredentials("OpenRouter rejected the API key")
		case e.Code == "quota_response_too_large":
			return invalidQuota()
		case e.Code == "invalid_proxy":
			return e
		}
	}
	return quotaUnavailable()
}

func get(ctx context.Context, client httpDoer, url, key string) ([]byte, error) {
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if err != nil {
		return nil, fetchError(err)
	}
	return resp.Body, nil
}

// fetchKey reads GET /key with the inference key.
func fetchKey(ctx context.Context, client httpDoer, base, apiKey string, now time.Time) (keyReading, error) {
	body, err := get(ctx, client, base+"/key", apiKey)
	if err != nil {
		return keyReading{}, err
	}
	return parseKey(body, now)
}

// fetchCredits reads GET /credits with the regular API key. OpenRouter
// documents this endpoint as management-key only, but it answers the regular
// key too (observed live 2026-11-01); any failure only omits the balance.
func fetchCredits(ctx context.Context, client httpDoer, base, apiKey string) (*account, error) {
	body, err := get(ctx, client, base+"/credits", apiKey)
	if err != nil {
		return nil, err
	}
	return parseCredits(body)
}

// newQuotaTransport is the kit's HTTP/1.1-only proxy-aware transport, so
// pooled /key and /credits connections never cross auth proxies.
func newQuotaTransport() *http.Transport { return kit.NewQuotaTransport() }
