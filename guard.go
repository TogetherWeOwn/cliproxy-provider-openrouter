package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// guard is the pre-dispatch spend guard: one per plugin instance, fail
// closed. It refuses a request locally, before any upstream call, when the
// key status cannot be read, the key has no credit limit (unless allowed),
// or the binding limit is at or below the auth's reserve.
//
// It keeps no local spend tally. OpenRouter enforces the key limit server
// side and the reserve is a soft margin. GET /key counters lag real spend, so
// the reserve can be overshot by everything spent before the counters catch
// up, not only by one 60-second reading.
//
// Because a fresh reading can still show headroom after OpenRouter reported
// a spent limit, every upstream exhaustion also sets a per-auth latch
// (latch.go). The host scopes every plugin failure to one (auth, model) pair;
// the latch makes each other model of the same auth fail here, at no upstream
// cost, and then cool down until the reset as well.
type guard struct {
	keys      *keyState
	hostProxy atomic.Pointer[string] // host proxy-url; nil until first seen
	now       func() time.Time
}

func newGuard(keys *keyState) *guard { return &guard{keys: keys, now: time.Now} }

// observeHost records the host's global proxy-url, which executor requests
// do not carry. Auth parse/refresh, model and quota requests deliver it.
func (g *guard) observeHost(h pluginapi.HostConfigSummary) {
	if g != nil {
		p := strings.TrimSpace(h.ProxyURL)
		g.hostProxy.Store(&p)
	}
}

// proxy resolves the auth's proxy_url, else the host's proxy-url. ok is false
// until the host setting has been seen: the read is refused rather than sent
// direct.
func (g *guard) proxy(storage []byte, metadata map[string]any) (string, bool) {
	if p := kit.AuthProxyURL(storage, metadata, ""); p != "" {
		return p, true
	}
	if h := g.hostProxy.Load(); h != nil {
		return *h, true
	}
	return "", false
}

func quotaUnverified() *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_unverified", Message: "quota_unverified: OpenRouter key status could not be read", HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: 60}
}

func keyLimitRequired() *pluginabi.Error {
	return &pluginabi.Error{Code: "key_limit_required", Message: "key_limit_required: set a credit limit on this OpenRouter key or allow_unlimited_key", HTTPStatus: http.StatusForbidden}
}

func quotaExhausted(message string, retry int) *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_exhausted", Message: "quota_exhausted: " + message, HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: retry}
}

// admit runs before every Execute, ExecuteStream and HttpRequest dispatch.
func (g *guard) admit(ctx context.Context, authID string, c credentials, storage []byte, metadata map[string]any, model string) error {
	proxy, ok := g.proxy(storage, metadata)
	if !ok {
		return quotaUnverified()
	}
	reading, err := g.keys.get(kit.WithProxyURL(ctx, proxy), authID, c, lastGoodMaxAge)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var e *pluginabi.Error
		if errors.As(err, &e) && e.Code == "invalid_credentials" {
			return e
		}
		return quotaUnverified()
	}
	now := g.now()
	if err = decide(reading, c, model, now); err != nil {
		return err
	}
	if until, scope, ok := g.keys.latched(authID, reading, model, now); ok {
		message := "OpenRouter reported a spent limit; waiting for its reset"
		if scope == latchFree {
			message = "OpenRouter daily free-model requests used up"
		}
		return quotaExhausted(message, retrySeconds(until, now))
	}
	return nil
}

// limitLayer is one spend cap that can block the key: the key limit, the
// effective budget or the account balance.
type limitLayer struct {
	remaining float64
	reset     time.Time
	resets    bool
}

// decide applies rules G1-G4 to a reading. G0 (no usable reading) is
// handled by admit.
func decide(r keyReading, c credentials, model string, now time.Time) error {
	if r.Management {
		return managementKeyError()
	}
	if r.Limit == nil && !c.allowUnlimited {
		return keyLimitRequired()
	}
	var layers []limitLayer
	if r.Limit != nil {
		if r.Remaining == nil {
			return quotaUnverified() // a limit without a remaining amount cannot be bounded
		}
		reset, ok := resolveReset(r.KeyResetsAt, r.Reset, now)
		layers = append(layers, limitLayer{*r.Remaining, reset, ok})
	}
	if r.Effective != nil {
		if rem := r.Effective.remaining(); rem != nil {
			reset, ok := resolveReset(r.Effective.ResetsAt, r.Effective.Interval, now)
			layers = append(layers, limitLayer{*rem, reset, ok})
		}
	}
	if r.Account != nil {
		layers = append(layers, limitLayer{remaining: r.Account.balance()})
	}
	binding, retry, blocked := 0.0, 0, false
	for i, l := range layers {
		if i == 0 || l.remaining < binding {
			binding = l.remaining
		}
		if l.remaining <= c.reserve {
			// Every blocking layer must have reset before a retry can pass.
			wait := recheckSeconds
			if l.resets {
				wait = retrySeconds(l.reset, now)
			}
			if !blocked || wait > retry {
				retry = wait
			}
			blocked = true
		}
	}
	if blocked {
		message := "OpenRouter credit limit reached"
		if binding > 0 {
			message = "OpenRouter credit reserve reached"
		}
		return quotaExhausted(message, retry)
	}
	if strings.HasSuffix(model, ":free") && r.Free != nil && r.Free.Limit > 0 && r.Free.Remaining <= 0 {
		return quotaExhausted("OpenRouter daily free-model requests used up", retrySeconds(nextUTCDay(now), now))
	}
	return nil
}
