// Package limiter implements the request-admission layers of the PLAN
// rate-limiting stack (PLAN §32–§35): a global request-rate limiter,
// per-key request-rate limiters, and per-key concurrency limits.
//
// Properties:
//
//   - token-bucket rate limiting with lazy refill (PLAN §34);
//   - non-blocking concurrency admission: exhaustion is rejected
//     immediately rather than queued, so a single key cannot build a
//     backlog on the admission path;
//   - all per-key state is created only for authenticated keys, so the
//     state bound is the finite key store (PLAN §35: one key must not
//     occupy the process);
//   - deterministic for a given clock: Allow takes the timestamp
//     explicitly, which keeps tests free of sleeps.
package limiter

import (
	"fmt"
	"sync"
	"time"

	"mellomting/internal/auth"
)

// Bucket is a lazy-refill token bucket (PLAN §34).
type Bucket struct {
	mu    sync.Mutex
	rate  float64 // tokens per second, > 0
	cap   float64 // burst capacity, >= 1
	avail float64
	last  time.Time
}

// NewBucket builds a bucket with the given sustained rate and burst
// capacity. A burst below 1 is clamped to 1 (a rate limit that could
// never admit a single request is a misconfiguration).
func NewBucket(rate float64, burst int) (*Bucket, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("limiter: rate must be > 0, got %v", rate)
	}
	if burst < 1 {
		burst = 1
	}
	// last is the zero time: the first Allow refills from the epoch,
	// which (capped at burst) leaves the bucket starting full. This
	// keeps the initial state deterministic and independent of
	// constructor timing.
	return &Bucket{rate: rate, cap: float64(burst), avail: 0, last: time.Time{}}, nil
}

// Allow consumes one token at time now. It reports ok and, when !ok,
// the duration until at least one token will be available (best-effort
// estimate for Retry-After, PLAN §34).
func (b *Bucket) Allow(now time.Time) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.After(b.last) {
		elapsed := now.Sub(b.last).Seconds()
		b.last = now
		b.avail += elapsed * b.rate
		if b.avail > b.cap {
			b.avail = b.cap
		}
	}
	if b.avail >= 1 {
		b.avail--
		return true, 0
	}
	ret := time.Duration(float64(time.Second) * (1 - b.avail) / b.rate)
	return false, ret
}

// Concurrency bounds the number of simultaneously in-flight requests
// of one key (PLAN §35). A limit of 0 means unbounded.
type Concurrency struct {
	ch chan struct{} // nil: unbounded
}

// NewConcurrency builds a concurrency bound; limit < 1 is unbounded.
func NewConcurrency(limit int) *Concurrency {
	if limit < 1 {
		return &Concurrency{}
	}
	return &Concurrency{ch: make(chan struct{}, limit)}
}

// Acquire takes one slot non-blockingly. It returns a release function
// (a no-op when unbounded) and ok; ok is false only when every slot is
// held, which the caller maps to a 429.
func (c *Concurrency) Acquire() (release func(), ok bool) {
	if c.ch == nil {
		return func() {}, true
	}
	select {
	case c.ch <- struct{}{}:
		return func() { <-c.ch }, true
	default:
		return func() {}, false
	}
}

// KeyState is the per-key admission state (PLAN §34, §35).
type KeyState struct {
	Bucket *Bucket      // per-key RPS; nil when the key sets no rate
	Concur *Concurrency // per-key concurrency; always non-nil (bounded or not)
}

// AllowRate checks the key's request-rate bucket. Keys without a rate
// limit are always admitted.
func (ks *KeyState) AllowRate(now time.Time) (bool, time.Duration) {
	if ks.Bucket == nil {
		return true, 0
	}
	return ks.Bucket.Allow(now)
}

// AcquireConcurrency takes a per-key in-flight slot.
func (ks *KeyState) AcquireConcurrency() (release func(), ok bool) {
	return ks.Concur.Acquire()
}

// Registry lazily materializes per-key limit state for authenticated
// keys (PLAN §32, §34, §35). State is bounded by the finite key store:
// only keys that passed authentication ever obtain a slot, and the
// number of such keys is the number of configured keys.
type Registry struct {
	mu   sync.Mutex
	keys map[string]*KeyState
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{keys: make(map[string]*KeyState)}
}

// For returns the limit state of a key, creating it on first use.
func (r *Registry) For(key *auth.Key) *KeyState {
	r.mu.Lock()
	defer r.mu.Unlock()
	ks, ok := r.keys[key.ID]
	if ok {
		return ks
	}
	ks = r.build(key)
	r.keys[key.ID] = ks
	return ks
}

func (r *Registry) build(key *auth.Key) *KeyState {
	ks := &KeyState{Concur: NewConcurrency(key.Limits.ConcurrentRequests)}
	if key.Limits.RequestsPerSecond > 0 {
		burst := key.Limits.Burst
		if burst < 1 {
			// One second of sustained rate (PLAN §34 spirit: a
			// burst must be calculable, default to something sane).
			burst = int(key.Limits.RequestsPerSecond)
			if burst < 1 {
				burst = 1
			}
		}
		b, err := NewBucket(key.Limits.RequestsPerSecond, burst)
		if err != nil { // validated at the key-store boundary; fail closed
			ks.Bucket = nil
			return ks
		}
		ks.Bucket = b
	}
	return ks
}

// Size is the number of materialized key states (test/operational aid).
func (r *Registry) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}
