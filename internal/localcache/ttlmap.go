// Package localcache replaces the Redis-backed TTL caches this codebase used
// when it ran as a multi-process, multi-host stack. On a single-machine
// native install there is exactly one radiusd process and one api process,
// so a value cached in Redis purely to be visible across replicas has no
// replicas left to be visible to — an in-process, mutex-protected map with
// the same TTL semantics is a direct substitute, with no network round trip.
//
// Every store here is a plain cache: an entry that disappears (eviction,
// process restart) degrades the caller to a cold lookup, never to an error.
// That mirrors how every one of the Redis stores this package replaces was
// already documented ("Redis is treated as an optimisation, never a
// dependency").
package localcache

import (
	"sync"
	"time"
)

// defaultSweepInterval bounds how long an expired entry can linger before a
// sweep reclaims it. Reads and writes already check expiry themselves, so
// this only bounds memory growth from keys nobody looks up again, not
// correctness.
const defaultSweepInterval = time.Minute

type entry[V any] struct {
	val     V
	expires time.Time // zero means "never set an expiry" — treated as already expired on read
}

// Store is a generic, mutex-protected TTL map.
type Store[V any] struct {
	mu      sync.RWMutex
	entries map[string]entry[V]
	stop    chan struct{}
	stopped sync.Once
}

// New constructs a Store with a background sweep goroutine running every
// sweepInterval. A zero or negative sweepInterval uses defaultSweepInterval.
func New[V any](sweepInterval time.Duration) *Store[V] {
	if sweepInterval <= 0 {
		sweepInterval = defaultSweepInterval
	}
	s := &Store[V]{
		entries: make(map[string]entry[V]),
		stop:    make(chan struct{}),
	}
	go s.sweepLoop(sweepInterval)
	return s
}

// Get returns the value stored under key, if present and not expired.
func (s *Store[V]) Get(key string) (V, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok || e.expires.IsZero() || time.Now().After(e.expires) {
		var zero V
		return zero, false
	}
	return e.val, true
}

// Set stores val under key with the given TTL. A zero or negative ttl stores
// an already-expired entry (matches Redis SET with EX 0 behaving as a no-op
// read-back) — callers in this codebase always pass a positive TTL.
func (s *Store[V]) Set(key string, val V, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = entry[V]{val: val, expires: time.Now().Add(ttl)}
}

// TrySet stores val under key only if no live (unexpired) entry already
// exists, reporting whether it did — the in-process equivalent of Redis
// SETNX. Safe under concurrent callers: the check and the write happen under
// the same lock, so two goroutines racing on the same key cannot both "win".
func (s *Store[V]) TrySet(key string, val V, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && !e.expires.IsZero() && time.Now().Before(e.expires) {
		return false
	}
	s.entries[key] = entry[V]{val: val, expires: time.Now().Add(ttl)}
	return true
}

// Delete removes key, if present.
func (s *Store[V]) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// TTL reports how long remains before key expires. Zero means the key is
// absent or already expired — cases callers have no reason to distinguish,
// since neither is "expiring soon". Intended for tests and diagnostics.
func (s *Store[V]) TTL(key string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok || e.expires.IsZero() {
		return 0
	}
	if remaining := time.Until(e.expires); remaining > 0 {
		return remaining
	}
	return 0
}

// Keys returns the live (unexpired) keys, in no particular order. Intended
// for tests and diagnostics — sort the result before comparing.
func (s *Store[V]) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	keys := make([]string, 0, len(s.entries))
	for k, e := range s.entries {
		if !e.expires.IsZero() && now.Before(e.expires) {
			keys = append(keys, k)
		}
	}
	return keys
}

// ExpireNow forces key to be treated as already expired, without deleting it
// outright. This is what a test uses in place of advancing a clock: the
// entry stops being visible to Get, exactly as a naturally-elapsed TTL
// would make it.
func (s *Store[V]) ExpireNow(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return
	}
	e.expires = time.Now().Add(-time.Second)
	s.entries[key] = e
}

// Len reports how many live (unexpired) entries the store holds. Intended
// for tests and diagnostics — callers should not branch on it, since an
// entry can expire between this call and the next Get.
func (s *Store[V]) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range s.entries {
		if !e.expires.IsZero() && now.Before(e.expires) {
			n++
		}
	}
	return n
}

// Close stops the background sweep goroutine. Safe to call more than once.
func (s *Store[V]) Close() {
	s.stopped.Do(func() { close(s.stop) })
}

func (s *Store[V]) sweepLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.sweep(now)
		}
	}
}

func (s *Store[V]) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if e.expires.IsZero() || now.After(e.expires) {
			delete(s.entries, k)
		}
	}
}
