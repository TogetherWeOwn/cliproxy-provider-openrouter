package main

import (
	"context"
	"sync"
	"time"
)

const (
	// keyTTL is the maximum age of a reading used without a re-read.
	keyTTL = 60 * time.Second
	// lastGoodMaxAge bounds how old a reading the guard may fall back to when
	// a re-read fails. Older than that, inference fails closed.
	lastGoodMaxAge = 10 * time.Minute
)

// keyState caches the last successful keyReading per auth for the guard and
// the quota card. Reads are single-flight per auth; errors are never cached.
type keyState struct {
	client httpDoer
	// base overrides the upstream base URL; tests point it at loopback.
	base string
	now  func() time.Time
	// creditsTimeout bounds GET /credits (see read).
	creditsTimeout time.Duration
	// onInvalidate also drops the quota card's cached copy.
	onInvalidate func(authID string)
	mu           sync.Mutex
	entries      map[string]*keyEntry
}

type keyEntry struct {
	digest [32]byte
	good   *keyReading
	// stale is set by invalidate: the next use must re-read, and the old
	// reading may no longer serve as a fallback.
	stale  bool
	gen    uint64
	flight *keyFlight
	// latches hold upstream exhaustions (see latch.go), by scope.
	latches [effectKinds]*latch
}

type keyFlight struct {
	done    chan struct{}
	gen     uint64 // entry generation when the read started
	reading keyReading
	err     error
}

func newKeyState(client httpDoer) *keyState {
	return &keyState{client: client, now: time.Now, creditsTimeout: defaultCreditsTimeout, entries: make(map[string]*keyEntry)}
}

func (k *keyState) endpoint() string {
	if k.base != "" {
		return k.base
	}
	return selectedBase(upstreamBase)
}

// entryLocked returns the auth's entry, starting over when the credential
// behind the auth ID changed.
func (k *keyState) entryLocked(authID string, c credentials) *keyEntry {
	d := c.digest()
	e := k.entries[authID]
	if e == nil || e.digest != d {
		e = &keyEntry{digest: d}
		k.entries[authID] = e
	}
	return e
}

// get returns a reading at most keyTTL old, re-reading GET /key and GET
// /credits when needed. If the re-read fails and maxStale > 0, the last good
// reading is returned while it is at most maxStale old and has not been
// invalidated. A shared read that an invalidation overtook is not returned:
// the caller reads again (bounded). ctx must carry the proxy setting
// (kit.WithProxyURL); a waiter's cancellation never fails the shared read.
func (k *keyState) get(ctx context.Context, authID string, c credentials, maxStale time.Duration) (keyReading, error) {
	for attempt := 1; ; attempt++ {
		r, superseded, err := k.getOnce(ctx, authID, c, maxStale)
		if !superseded {
			return r, err
		}
		if attempt >= maxReadAttempts {
			// Every read was overtaken by a limit error or a reset: fail closed.
			return keyReading{}, quotaUnavailable()
		}
	}
}

// boundary is the first reset after the reading was taken: the next 00:00
// UTC (every OpenRouter calendar window and the free-model day reset then),
// or an earlier server-reported reset of the key limit or the effective
// budget. A reading never outlives it: past a reset, its counts are wrong.
func (r keyReading) boundary() time.Time {
	b := nextUTCDay(r.FetchedAt)
	for _, at := range []*time.Time{r.KeyResetsAt, r.effectiveResetsAt()} {
		if at != nil && at.After(r.FetchedAt) && at.Before(b) {
			b = *at
		}
	}
	return b
}

func (r keyReading) effectiveResetsAt() *time.Time {
	if r.Effective == nil {
		return nil
	}
	return r.Effective.ResetsAt
}

// maxReadAttempts bounds re-reads after invalidations or resets overtook a
// shared read.
const maxReadAttempts = 3

func (k *keyState) getOnce(ctx context.Context, authID string, c credentials, maxStale time.Duration) (keyReading, bool, error) {
	k.mu.Lock()
	e := k.entryLocked(authID, c)
	if e.good != nil && !e.stale && k.now().Sub(e.good.FetchedAt) < keyTTL && k.now().Before(e.good.boundary()) {
		r := *e.good
		k.mu.Unlock()
		return r, false, nil
	}
	f := e.flight
	if f == nil {
		f = &keyFlight{done: make(chan struct{}), gen: e.gen}
		e.flight = f
		k.mu.Unlock()
		start := k.now()
		reading, err := k.read(context.WithoutCancel(ctx), c, start)
		k.mu.Lock()
		f.reading, f.err = reading, err
		// An invalidated or older read never replaces a newer reading.
		if err == nil && k.entries[authID] == e && e.gen == f.gen && (e.good == nil || !e.good.FetchedAt.After(start)) {
			e.good, e.stale = &reading, false
		}
		if e.flight == f {
			e.flight = nil
		}
		close(f.done)
		k.mu.Unlock()
	} else {
		k.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return keyReading{}, false, ctx.Err()
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if f.err == nil {
		// A read that started before an invalidation may predate the limit
		// error, and one that a reset overtook (a slow /key or /credits
		// around 00:00 UTC) holds the old window's counts: neither may admit.
		overtaken := k.entries[authID] == e && e.gen != f.gen
		return f.reading, overtaken || !k.now().Before(f.reading.boundary()), nil
	}
	if maxStale > 0 && e.good != nil && !e.stale && k.now().Sub(e.good.FetchedAt) <= maxStale && k.now().Before(e.good.boundary()) {
		return *e.good, false, nil
	}
	return keyReading{}, false, f.err
}

// defaultCreditsTimeout bounds GET /credits, which runs next to GET /key: the
// balance is optional, so a slow /credits must not hold up admission long.
const defaultCreditsTimeout = 5 * time.Second

// read fetches GET /key and, concurrently, GET /credits, both with the
// regular API key. Any /credits failure (401, 403, 5xx, timeout, bad body)
// only omits the account balance.
func (k *keyState) read(ctx context.Context, c credentials, start time.Time) (keyReading, error) {
	base := k.endpoint()
	// /credits is never cancelled early, even when /key fails: cancelling an
	// in-flight request on the shared HTTP/1.1 pool can break the next read.
	// Its own timeout bounds it.
	creditsCtx, cancel := context.WithTimeout(ctx, k.creditsTimeout)
	credits := make(chan *account, 1)
	go func() {
		defer cancel()
		a, err := fetchCredits(creditsCtx, k.client, base, c.apiKey)
		if err != nil {
			a = nil
		}
		credits <- a
	}()
	reading, err := fetchKey(ctx, k.client, base, c.apiKey, start)
	if err != nil {
		return keyReading{}, err
	}
	reading.Account = <-credits
	return reading, nil
}

// peek returns the last good reading, if any, without a network read. It is
// a classification hint only.
func (k *keyState) peek(authID string) *keyReading {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.entries[authID]; e != nil && e.good != nil {
		r := *e.good
		return &r
	}
	return nil
}

// invalidate forces the next use to re-read after an upstream limit error.
func (k *keyState) invalidate(authID string) {
	k.mu.Lock()
	if e := k.entries[authID]; e != nil {
		e.stale, e.flight = true, nil
		e.gen++
	}
	k.mu.Unlock()
	if k.onInvalidate != nil {
		k.onInvalidate(authID)
	}
}
