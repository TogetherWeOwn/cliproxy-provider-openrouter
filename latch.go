package main

import (
	"strings"
	"time"
)

// OpenRouter's GET /key counters lag real spend (observed live 2026-11-01:
// two paid requests still read usage 0 seconds later). So after OpenRouter
// itself reports a spent limit, a fresh reading can still show headroom, and
// the guard would keep sending requests that fail upstream. A latch remembers
// the upstream exhaustion per auth and refuses locally until its retry time,
// unless a later reading shows the owner changed the cap (raised or removed
// the key limit, changed its reset window or the binding budget, or topped up
// the account). A reading that merely still shows money left is not enough.

// latchScope says which models of an auth a latch refuses.
type latchScope int

const (
	// latchSpend: a key limit, budget or credit error; every model.
	latchSpend latchScope = iota
	// latchFree: the daily free-model requests are used up; ":free" models.
	latchFree
	latchScopes
)

type latch struct {
	until time.Time
	// base is the reading the exhaustion is compared against; nil until the
	// first reading after the error when no reading existed before it.
	base *capShape
}

// capShape is what the owner can change to lift an exhaustion before its
// reset. Spend counters are not part of it: they lag.
type capShape struct {
	limit   *float64
	reset   window
	budget  *float64 // effective_budget.limit_usd
	credits *float64 // /credits total_credits
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
	return s
}

func eqNum(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// changed reports an owner action since base. A missing /credits reading on
// either side is not a change: that read is optional and may fail.
func (base capShape) changed(now capShape) bool {
	return !eqNum(base.limit, now.limit) || base.reset != now.reset || !eqNum(base.budget, now.budget) ||
		(base.credits != nil && now.credits != nil && *base.credits != *now.credits)
}

// latch records an upstream exhaustion of scope on authID until the given
// time. A longer existing latch is kept.
func (k *keyState) latch(authID string, scope latchScope, until time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.entries[authID]
	if e == nil {
		return
	}
	if l := e.latches[scope]; l != nil && l.until.After(until) {
		until = l.until
	}
	l := &latch{until: until}
	if e.good != nil {
		base := shapeOf(*e.good)
		l.base = &base
	}
	e.latches[scope] = l
}

// latched returns the end and scope of the longest latch that refuses model
// on authID given the reading r, clearing latches that expired or that r
// shows were lifted.
func (k *keyState) latched(authID string, r keyReading, model string, now time.Time) (until time.Time, binding latchScope, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.entries[authID]
	if e == nil {
		return time.Time{}, 0, false
	}
	for scope, l := range e.latches {
		if l == nil || (latchScope(scope) == latchFree && !strings.HasSuffix(model, ":free")) {
			continue
		}
		shape := shapeOf(r)
		switch {
		case !now.Before(l.until), l.base != nil && l.base.changed(shape):
			e.latches[scope] = nil
			continue
		case l.base == nil:
			l.base = &shape
		}
		if l.until.After(until) {
			until, binding = l.until, latchScope(scope)
		}
	}
	return until, binding, !until.IsZero()
}
