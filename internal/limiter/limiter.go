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
	ch    chan struct{} // nil: unbounded
	limit int           // 0: unbounded; recorded so a reload can carry an unchanged bound
}

// NewConcurrency builds a concurrency bound; limit < 1 is unbounded.
func NewConcurrency(limit int) *Concurrency {
	if limit < 1 {
		return &Concurrency{}
	}
	return &Concurrency{ch: make(chan struct{}, limit), limit: limit}
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

// match reports whether the bucket was built with the given rate and
// burst, i.e. whether carrying it across a reload is safe (FIX-22).
func (b *Bucket) match(rate float64, burst int) bool {
	return b.rate == rate && b.cap == float64(burst)
}

// Registry lazily materializes per-key limit state for authenticated
// keys (PLAN §32, §34, §35). State is bounded by the finite key store:
// only keys that passed authentication ever obtain a slot, and the
// number of such keys is the number of configured keys.
type Registry struct {
	mu    sync.Mutex
	keys  map[string]*KeyState
	carry map[string]*Bucket // buckets carried from the prior generation
	// carryConc holds the prior generation's concurrency bounds; one is
	// reused only when the key's limit is unchanged, so in-flight slots
	// stay accounted across a reload.
	carryConc map[string]*Concurrency
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{keys: make(map[string]*KeyState)}
}

// NewRegistryCarrying builds an empty registry that carries over the
// token buckets of the previous generation for keys whose rate and burst
// are unchanged, so a SIGHUP reload does not gift every key a fresh
// burst (FIX-22, PLAN §30, §34).
//
// A concurrency bound is carried on the same terms: only when
// concurrent_requests is unchanged. A changed limit still applies
// immediately, because a changed bound is rebuilt. Rebuilding an
// UNCHANGED bound would hand the key a second full set of slots while
// the previous generation's in-flight requests still hold release
// closures bound to the old channel, so a reload — the common case being
// key rotation, which changes no limit at all — transiently allowed up
// to twice concurrent_requests.
//
// Only the carryable buckets are retained, as an immutable snapshot; the
// previous Registry itself is deliberately not referenced, so a chain of
// SIGHUPs never accumulates a linked list of dead generations (R3,
// FIX-22 eval).
func NewRegistryCarrying(prev *Registry) *Registry {
	var carry map[string]*Bucket
	var carryConc map[string]*Concurrency
	if prev != nil {
		prev.mu.Lock()
		carry = make(map[string]*Bucket, len(prev.carry)+len(prev.keys))
		carryConc = make(map[string]*Concurrency, len(prev.carryConc)+len(prev.keys))
		for id, b := range prev.carry {
			carry[id] = b
		}
		for id, c := range prev.carryConc {
			carryConc[id] = c
		}
		if len(prev.keys) > 0 {
			for id, ks := range prev.keys {
				// Materialized state supersedes older carried settings,
				// including a rate limit that has since been removed.
				delete(carry, id)
				delete(carryConc, id)
				if ks.Bucket != nil {
					carry[id] = ks.Bucket
				}
				if ks.Concur != nil {
					carryConc[id] = ks.Concur
				}
			}
		}
		prev.mu.Unlock()
	}
	return &Registry{keys: make(map[string]*KeyState), carry: carry, carryConc: carryConc}
}

// Retain drops state for keys removed from the current store. Call before
// publishing a reloaded registry so repeated rotations remain bounded by
// the current key set rather than retaining every historical key.
func (r *Registry) Retain(keep func(string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.carry {
		if !keep(id) {
			delete(r.carry, id)
		}
	}
	for id := range r.carryConc {
		if !keep(id) {
			delete(r.carryConc, id)
		}
	}
	for id := range r.keys {
		if !keep(id) {
			delete(r.keys, id)
		}
	}
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
	if b, ok := r.carry[key.ID]; ok {
		// Carry only when the effective burst is unchanged, not merely
		// the raw field: a bucket built with burst omitted (derived from
		// the rate) must still match a reload where the burst remains
		// omitted, or every reload re-gifts a fresh burst (FIX-22).
		burst := derivedBurst(key.Limits.RequestsPerSecond, key.Limits.Burst)
		if b.match(key.Limits.RequestsPerSecond, burst) {
			ks.Bucket = b
		}
	}
	r.keys[key.ID] = ks
	return ks
}

// derivedBurst computes the effective burst a bucket is built with:
// the configured value when present, else one second of sustained rate
// (at least one), mirroring build().
func derivedBurst(rate float64, burst int) int {
	if burst < 1 {
		burst = max(int(rate), 1)
	}
	return burst
}

func (r *Registry) build(key *auth.Key) *KeyState {
	// Reuse the prior generation's bound when the limit is unchanged, so
	// its in-flight holders and this generation's share one set of slots.
	conc := r.carryConc[key.ID]
	want := key.Limits.ConcurrentRequests
	if want < 1 {
		want = 0
	}
	if conc == nil || conc.limit != want {
		conc = NewConcurrency(key.Limits.ConcurrentRequests)
	}
	ks := &KeyState{Concur: conc}
	if key.Limits.RequestsPerSecond > 0 {
		burst := derivedBurst(key.Limits.RequestsPerSecond, key.Limits.Burst)
		b, err := NewBucket(key.Limits.RequestsPerSecond, burst)
		if err != nil { // unreachable for store-validated keys: negative rates are
			// rejected at the key-store boundary (auth.validateLimits), so this
			// is a defensive fallback only; fail open rather than panic
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

// SourceState is the pre-auth admission state of one source IP (PLAN
// §33): a per-source request-rate bucket checked before authentication.
type SourceState struct {
	rate *Bucket // per-source RPS
	last time.Time
}

// SourceRegistry is a bounded, lazily-populated per-source pre-auth
// limiter (PLAN §33). Unlike the key registry, sources are not bounded
// by the key store: any IP can knock on the door, so the map is capped
// and, when full, the least-recently-touched entry is evicted. A flood
// of distinct addresses therefore cannot grow the process's memory
// without bound, and a single host's flood is throttled before it can
// drain the shared authenticated bucket.
type SourceRegistry struct {
	mu    sync.Mutex
	cap   int
	rate  float64
	burst int
	srcs  map[string]*SourceState
}

// NewSourceRegistry builds a bounded per-source pre-auth limiter. rate
// must be > 0; cap is the maximum number of distinct sources held.
func NewSourceRegistry(rate float64, burst, cap int) (*SourceRegistry, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("limiter: preauth rate must be > 0, got %v", rate)
	}
	if burst < 1 {
		burst = 1
	}
	if cap < 1 {
		cap = 1
	}
	return &SourceRegistry{
		cap:   cap,
		rate:  rate,
		burst: burst,
		srcs:  make(map[string]*SourceState, cap),
	}, nil
}

// Allow consumes one pre-auth token for the source at time now. It
// reports ok and, when !ok, the duration until a token is available.
func (r *SourceRegistry) Allow(ip string, now time.Time) (ok bool, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.srcs[ip]
	if !ok {
		if len(r.srcs) >= r.cap {
			r.evictOldest()
		}
		b, err := NewBucket(r.rate, r.burst)
		if err != nil { // unreachable: constructor validated the rate
			return true, 0
		}
		st = &SourceState{rate: b}
		r.srcs[ip] = st
	}
	st.last = now
	return st.rate.Allow(now)
}

// evictProbeBudget bounds the work of one source eviction: when the map
// is at its cap, Allow probes at most this many sources and evicts the
// least-recently-touched one it saw (FIX-17), keeping eviction O(1)-ish
// at the DefaultPreauthSources bound instead of a full-map scan per new
// source.
const evictProbeBudget = 64

// evictOldest drops a source so the map stays bounded. It must be called
// with r.mu held.
func (r *SourceRegistry) evictOldest() {
	var oldest string
	var oldestTime time.Time
	first := true
	probed := 0
	for ip, st := range r.srcs {
		probed++
		if first || st.last.Before(oldestTime) {
			oldest, oldestTime, first = ip, st.last, false
		}
		if probed >= evictProbeBudget {
			break
		}
	}
	delete(r.srcs, oldest)
}

// Size is the number of materialized source states (test/operational
// aid).
func (r *SourceRegistry) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.srcs)
}
