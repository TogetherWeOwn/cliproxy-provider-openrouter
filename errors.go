package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	maxErrorBody = 64 << 10
	// inFlightRetry is the default wait for a transient in-flight budget 402
	// without Retry-After (the host floors 429 cooldowns at 10 s).
	inFlightRetry = 5
)

// upstreamError is the part of an OpenRouter error body the classifier
// branches on. Text is never copied into a public error.
type upstreamError struct {
	code                                  int
	hasCode                               bool
	message                               string // lowercased, for matching only
	limitSource, reason, errorType, scope string
	resetsAt                              *time.Time
}

func lower(o object, name string) string {
	v, _ := o.str(name)
	if v == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*v))
}

// parseUpstreamError reads at most the first 64 KiB of an error body. ok is
// false when that is not a JSON object.
func parseUpstreamError(body []byte) (upstreamError, bool) {
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	top, ok := decodeObject(body)
	if !ok {
		return upstreamError{}, false
	}
	var u upstreamError
	e, ok := top.child("error")
	if !ok {
		return u, true
	}
	if raw, ok := e["code"]; ok {
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			if v, err := strconv.Atoi(string(n)); err == nil {
				u.code, u.hasCode = v, true
			}
		} else if s := lower(e, "code"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				u.code, u.hasCode = v, true
			}
		}
	}
	u.message = lower(e, "message")
	if m, ok := e.child("metadata"); ok {
		u.limitSource, u.reason, u.errorType, u.scope = lower(m, "limit_source"), lower(m, "reason"), lower(m, "error_type"), lower(m, "scope")
		at, _ := m.str("resets_at")
		u.resetsAt = parseInstant(at)
	}
	return u, true
}

// rateLimitReset reads X-RateLimit-Reset. Its unit is undocumented: values
// from 1e12 are epoch milliseconds, from 1e9 epoch seconds, else a delta in
// seconds. A past or invalid value yields 0 (host backoff).
func rateLimitReset(value string, now time.Time) int {
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	var wait float64
	switch {
	case v >= 1e12:
		wait = v/1000 - float64(now.UnixNano())/1e9
	case v >= 1e9:
		wait = v - float64(now.UnixNano())/1e9
	default:
		wait = v
	}
	if wait <= 0 {
		return 0
	}
	return int(math.Min(math.Ceil(wait), maxRetry.Seconds()))
}

var policyErrorTypes = map[string]bool{"content_policy_violation": true, "moderation": true, "refusal": true}

func failure(status int, code, message string, retry int) *pluginabi.Error {
	return &pluginabi.Error{Code: code, Message: code + ": " + message, HTTPStatus: status, RetryAfterSeconds: retry, Retryable: status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500}
}

// resetRetry is the wait until a known reset, else the hourly re-check.
func resetRetry(server *time.Time, w window, now time.Time) int {
	if reset, ok := resolveReset(server, w, now); ok {
		return retrySeconds(reset, now)
	}
	return recheckSeconds
}

// effect is what an upstream failure means for the auth's key state.
type effect int

// Every effect except effectNone re-reads GET /key before the next
// admission. From effectKeyLimit on, the auth is also latched until the
// retry time (latch.go); the kind names the cap whose change lifts it.
const (
	effectNone effect = iota
	// effectReread: a transient or request-scoped limit result.
	effectReread
	// effectKeyLimit: the key's credit limit is spent (every model).
	effectKeyLimit
	// effectBudget: a guardrail or workspace budget is spent (every model).
	effectBudget
	// effectCredits: the account credits are spent (every model).
	effectCredits
	// effectLimit: another spend limit is reached (every model).
	effectLimit
	// effectFree: the daily free-model requests are used up (":free" only).
	effectFree
	effectKinds
)

// latches reports an exhaustion that latches the auth.
func (e effect) latches() bool { return e >= effectKeyLimit && e < effectKinds }

// accountWide reports an exhaustion that spends a cap shared by every model
// of the auth (key limit, guardrail/workspace budget, account credits or
// another spend limit). The daily free-model quota is not account-wide: it
// applies to ":free" models only, so scoping it would wrongly cool paid
// models. Host v8.0.63 cools the whole credential on a 429 whose error
// carries CredentialScoped until RetryAfterSeconds; the provider Code is
// unchanged.
func (e effect) accountWide() bool {
	return e == effectKeyLimit || e == effectBudget || e == effectCredits || e == effectLimit
}

// classifyFailure maps an upstream failure to a public error (see the README
// table) and its effect on the auth's key state. reading is the last good
// reading, model the requested model; both may be empty.
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time, reading *keyReading, model string) (*pluginabi.Error, effect) {
	status := resp.StatusCode
	retryAfter := kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now)
	u, isJSON := parseUpstreamError(resp.Body)
	// A known exhaustion is never shortened by a shorter Retry-After; a
	// longer server hint wins.
	exhausted := func(message string, retry int, kind effect) (*pluginabi.Error, effect) {
		e := quotaExhausted(message, max(retry, retryAfter))
		// The latch already refuses every model locally; marking the error
		// credential-scoped stops the host routing other models into the
		// latched credential until the reset. The flag is additive: Code,
		// Message, 429 status and the reset-derived RetryAfterSeconds keep
		// their exact prior shape.
		if kind.accountWide() {
			e.CredentialScoped = true
		}
		return e, kind
	}
	payment := status == http.StatusPaymentRequired
	limitStatus := payment || status == http.StatusForbidden
	switch {
	case limitStatus && u.limitSource == "openrouter_key_limit":
		w, server := windowUnknown, u.resetsAt
		if reading != nil {
			w = reading.Reset
			if server == nil {
				server = reading.KeyResetsAt
			}
		}
		return exhausted("OpenRouter key credit limit reached", resetRetry(server, w, now), effectKeyLimit)
	case limitStatus && u.limitSource == "openrouter_guardrail_budget":
		return exhausted("OpenRouter guardrail budget reached", resetRetry(u.resetsAt, windowUnknown, now), effectBudget)
	case limitStatus && u.limitSource == "openrouter_workspace_budget":
		return exhausted("OpenRouter workspace budget reached", resetRetry(u.resetsAt, windowUnknown, now), effectBudget)
	case limitStatus && (u.limitSource == "openrouter_in_flight_budget" || u.reason == "in_flight_budget_exhausted"):
		retry := retryAfter
		if retry <= 0 {
			retry = inFlightRetry
		}
		return failure(http.StatusTooManyRequests, "rate_limited", "OpenRouter in-flight spending budget is full", retry), effectReread
	case payment && u.limitSource == "openrouter_credits" && u.reason == "weight_exceeds_budget":
		return failure(http.StatusBadRequest, "request_exceeds_budget", "request is too expensive for the OpenRouter spending budget; lower max_tokens or the prompt", 0), effectReread
	case payment:
		return exhausted("OpenRouter account credits exhausted", recheckSeconds, effectCredits)
	case status == http.StatusForbidden && u.limitSource != "":
		return exhausted("OpenRouter spend limit reached", resetRetry(u.resetsAt, windowUnknown, now), effectLimit)
	case status == http.StatusUnauthorized:
		return failure(status, "invalid_credentials", "OpenRouter rejected the API key", 0), effectNone
	case status == http.StatusForbidden && (policyErrorTypes[u.errorType] || strings.Contains(u.message, "moderation")):
		return failure(http.StatusBadRequest, "content_policy", "OpenRouter declined the request under a content policy", 0), effectNone
	case status == http.StatusForbidden:
		return failure(status, "entitlement_required", "OpenRouter denied access to the model or feature", 0), effectNone
	case status == http.StatusNotFound:
		return failure(status, "upstream_not_found", "model_not_supported or endpoint not found on OpenRouter", 0), effectNone
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity:
		return failure(status, "invalid_request", "OpenRouter rejected the request", 0), effectNone
	case status == http.StatusRequestTimeout:
		return failure(status, "timeout", "OpenRouter request timed out", retryAfter), effectNone
	case status == http.StatusTooManyRequests:
		// The daily free-model quota: named in the message, or a :free 429
		// while the last reading already showed none left (the counters lag,
		// so the message is the reliable signal).
		if strings.Contains(u.message, "free-models-per-day") || (strings.HasSuffix(model, ":free") && reading != nil && reading.Free != nil && reading.Free.Remaining <= 0) {
			return exhausted("OpenRouter daily free-model requests used up", retrySeconds(nextUTCDay(now), now), effectFree)
		}
		retry := retryAfter
		if retry <= 0 {
			retry = rateLimitReset(resp.Headers.Get("X-RateLimit-Reset"), now)
		}
		return failure(status, "rate_limited", "OpenRouter rate limit reached", retry), effectNone
	}
	code := "upstream_unavailable"
	switch status {
	case 500, 502, 503, 504, 524, 529:
		if !isJSON {
			code = "upstream_error"
		}
	default:
		code = "upstream_error"
	}
	return failure(status, code, "OpenRouter request failed", retryAfter), effectNone
}
