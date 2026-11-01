package main

import (
	"strings"
	"time"
)

// OpenRouter's GET /key counters lag real spend (observed live 2026-11-01:
// two paid requests still read usage 0 seconds later). So after OpenRouter
// itself reports a spent cap, a fresh reading can still show headroom, and
// the guard would keep sending requests that fail upstream. A latch remembers
// the upstream exhaustion per auth and refuses locally until its retry time,
// unless a later reading shows the owner changed the cap that failed. A
// reading that merely still shows money or requests left is not enough.
//
// Latches are kept per kind (the effect of the failure): a ":free" latch
// refuses only ":free" models, every other kind refuses every model.

type latch struct {
	until time.Time
	// base is the reading the exhaustion is compared against; nil until the
	// first reading after the error when no reading existed before it.
	base *capShape
}

// capShape is what the owner can change to lift an exhaustion before its
// reset. Spend and request counters are not part of it: they lag.
type capShape struct {
	limit     *float64 // key limit
	reset     window   // key limit_reset
	budget    *float64 // effective_budget.limit_usd
	credits   *float64 // /credits total_credits
	freeLimit *float64 // free_model_daily_requests.limit
}

func shapeOf(r keyReading) capShape {
	s := capShape{limit: r.Limit, reset: r.Reset}
	if r.Effective != nil {
		s.budget = r.Effective.Limit
	}
	if r.Account != nil {
		credits := r.Account.Credits
		s.credits = &credits
	}
	if r.Free != nil {
		limit := float64(r.Free.Limit)
		s.freeLimit = &limit
	}
	return s
}

func eqNum(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// raised reports a value present on both sides that went up. A missing value
// (an optional read that failed) is never a change.
func raised(before, after *float64) bool { return before != nil && after != nil && *after > *before }

// lifted reports an owner change since base to the cap a latch of this kind
// stands for. Adding account credits does not lift a spent key limit, and a
// changed key limit does not restore the daily free-model requests.
func (base capShape) lifted(kind effect, now capShape) bool {
	keyChanged := !eqNum(base.limit, now.limit) || base.reset != now.reset
	budgetChanged := !eqNum(base.budget, now.budget)
	switch kind {
	case effectKeyLimit:
		return keyChanged
	case effectBudget:
		return budgetChanged
	case effectCredits:
		return raised(base.credits, now.credits)
	case effectFree:
		return raised(base.freeLimit, now.freeLimit)
	}
	return keyChanged || budgetChanged || raised(base.credits, now.credits)
}

// latch records an upstream exhaustion of kind on authID until the given
// time. A longer existing latch of the same kind is kept.
func (k *keyState) latch(authID string, kind effect, until time.Time) {
	if !kind.latches() {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.entries[authID]
	if e == nil {
		return
	}
	if l := e.latches[kind]; l != nil && l.until.After(until) {
		until = l.until
	}
	l := &latch{until: until}
	if e.good != nil {
		base := shapeOf(*e.good)
		l.base = &base
	}
	e.latches[kind] = l
}

// latched returns the end and kind of the longest latch that refuses model
// on authID given the reading r, clearing latches that expired or that r
// shows were lifted.
func (k *keyState) latched(authID string, r keyReading, model string, now time.Time) (until time.Time, binding effect, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.entries[authID]
	if e == nil {
		return time.Time{}, effectNone, false
	}
	shape := shapeOf(r)
	for i, l := range e.latches {
		kind := effect(i)
		if l == nil || (kind == effectFree && !strings.HasSuffix(model, ":free")) {
			continue
		}
		switch {
		case !now.Before(l.until), l.base != nil && l.base.lifted(kind, shape):
			e.latches[i] = nil
			continue
		case l.base == nil:
			base := shape
			l.base = &base
		}
		if l.until.After(until) {
			until, binding = l.until, kind
		}
	}
	return until, binding, !until.IsZero()
}
