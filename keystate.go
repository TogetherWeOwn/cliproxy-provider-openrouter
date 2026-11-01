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
}

type keyFlight struct {
	done    chan struct{}
	reading keyReading
	err     error
}

func newKeyState(client httpDoer) *keyState {
	return &keyState{client: client, now: time.Now, entries: make(map[string]*keyEntry)}
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

// get returns a reading at most keyTTL old, re-reading GET /key (and GET
// /credits with a management key) when needed. If the re-read fails and
// maxStale > 0, the last good reading is returned while it is at most
// maxStale old and has not been invalidated. ctx must carry the proxy
// setting (kit.WithProxyURL); a waiter's cancellation never fails the shared
// read.
func (k *keyState) get(ctx context.Context, authID string, c credentials, maxStale time.Duration) (keyReading, error) {
	k.mu.Lock()
	e := k.entryLocked(authID, c)
	if e.good != nil && !e.stale && k.now().Sub(e.good.FetchedAt) < keyTTL {
		r := *e.good
		k.mu.Unlock()
		return r, nil
	}
	f := e.flight
	if f == nil {
		f = &keyFlight{done: make(chan struct{})}
		e.flight = f
		gen := e.gen
		k.mu.Unlock()
		start := k.now()
		reading, err := k.read(context.WithoutCancel(ctx), c, start)
		k.mu.Lock()
		f.reading, f.err = reading, err
		// An invalidated or older read never replaces a newer reading.
		if err == nil && k.entries[authID] == e && e.gen == gen && (e.good == nil || !e.good.FetchedAt.After(start)) {
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
			return keyReading{}, ctx.Err()
		}
	}
	if f.err == nil {
		return f.reading, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if maxStale > 0 && e.good != nil && !e.stale && k.now().Sub(e.good.FetchedAt) <= maxStale {
		return *e.good, nil
	}
	return keyReading{}, f.err
}

func (k *keyState) read(ctx context.Context, c credentials, start time.Time) (keyReading, error) {
	reading, err := fetchKey(ctx, k.client, k.endpoint(), c.apiKey, start)
	if err != nil {
		return keyReading{}, err
	}
	// The account balance is optional: any /credits failure (including the
	// 403 for a non-management key) only omits it.
	if c.managementKey != "" {
		if a, err := fetchCredits(ctx, k.client, k.endpoint(), c.managementKey); err == nil {
			reading.Account = a
		}
	}
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
