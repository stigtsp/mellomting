package limiter

import (
	"testing"
	"time"

	"mellomting/internal/auth"
)

func TestBucketBurstAndRefill(t *testing.T) {
	t.Parallel()
	t0 := time.Now()
	b, err := NewBucket(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if ok, _ := b.Allow(t0); !ok {
			t.Fatalf("burst of 3 must admit 3 immediately (admitted %d)", i)
		}
	}
	if ok, ra := b.Allow(t0); ok {
		t.Fatal("fourth immediate request must be rejected")
	} else if ra <= 0 || ra > 500*time.Millisecond {
		t.Fatalf("retryAfter = %v (want about 500ms at 2/s)", ra)
	}
	// After 500ms at 2/s: one token refilled.
	if ok, _ := b.Allow(t0.Add(500 * time.Millisecond)); !ok {
		t.Fatal("refilled token not admitted after 500ms")
	}
	// Refill is capped at the burst: a burst-1 bucket refills at most
	// one token however long it waits (no unbounded accumulation).
	b1, err := NewBucket(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := b1.Allow(t0); !ok {
		t.Fatal("fresh burst-1 bucket must admit")
	}
	if ok, ra := b1.Allow(t0); ok || ra <= 0 || ra > 500*time.Millisecond {
		t.Fatalf("second immediate request: ok=%v ra=%v", ok, ra)
	}
	if ok, _ := b1.Allow(t0.Add(time.Hour)); !ok {
		t.Fatal("capped bucket must admit one request after an hour")
	}
	if ok, _ := b1.Allow(t0.Add(time.Hour)); ok {
		t.Fatal("second request after an hour must be capped by the burst")
	}
}

func TestBucketRejectsBadConfig(t *testing.T) {
	t.Parallel()
	if _, err := NewBucket(0, 5); err == nil {
		t.Fatal("zero rate accepted")
	}
	if _, err := NewBucket(-1, 5); err == nil {
		t.Fatal("negative rate accepted")
	}
	// Burst below 1 is clamped, not an error.
	b, err := NewBucket(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Allow(time.Now()); !ok {
		t.Fatal("clamped burst unusable")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	t.Parallel()
	c := NewConcurrency(2)
	r1, ok1 := c.Acquire()
	r2, ok2 := c.Acquire()
	if !ok1 || !ok2 {
		t.Fatal("two slots must be granted")
	}
	_, ok3 := c.Acquire()
	if ok3 {
		t.Fatal("third slot must be rejected (PLAN §35: no unbounded queue)")
	}
	r1()
	r2()
	_, ok4 := c.Acquire()
	if !ok4 {
		t.Fatal("released slot must be reusable")
	}
	// Unbounded variant never rejects.
	u := NewConcurrency(0)
	for range 1000 {
		if _, ok := u.Acquire(); !ok {
			t.Fatal("unbounded limit rejected a request")
		}
	}
}

func TestRegistryPerKeyState(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	k1 := &auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}}
	k2 := &auth.Key{ID: "K2"} // no limits

	s1 := reg.For(k1)
	if s1.Bucket == nil || s1.Concur == nil {
		t.Fatal("limited key must get rate state")
	}
	s2 := reg.For(k1)
	if s1 != s2 {
		t.Fatal("registry must return the same state per key")
	}
	if reg.Size() != 1 {
		t.Fatalf("Size = %d, want 1", reg.Size())
	}

	s3 := reg.For(k2)
	if s3.Bucket != nil {
		t.Fatal("key without a rate limit must have no bucket")
	}
	now := time.Now()
	if ok, _ := s3.AllowRate(now); !ok {
		t.Fatal("unlimited key must be admitted")
	}
	// Unbounded key can take many concurrency slots.
	for range 50 {
		release, ok := s3.AcquireConcurrency()
		if !ok {
			t.Fatal("unbounded concurrency rejected a request")
		}
		release()
	}
	if reg.Size() != 2 {
		t.Fatalf("Size = %d, want 2", reg.Size())
	}
}

func TestSourceRegistryPerSourceIsolation(t *testing.T) {
	t.Parallel()
	t0 := time.Now()
	r, err := NewSourceRegistry(1, 2, 16)
	if err != nil {
		t.Fatal(err)
	}
	// Source A bursts 2, then is throttled while source B is unaffected.
	if ok, _ := r.Allow("1.2.3.4", t0); !ok {
		t.Fatal("source A: first request admitted")
	}
	if ok, _ := r.Allow("1.2.3.4", t0); !ok {
		t.Fatal("source A: second (burst) request admitted")
	}
	if ok, _ := r.Allow("1.2.3.4", t0); ok {
		t.Fatal("source A: third immediate request must be throttled")
	}
	if ok, _ := r.Allow("5.6.7.8", t0); !ok {
		t.Fatal("source B must be unaffected by source A's throttle")
	}
	if r.Size() != 2 {
		t.Fatalf("Size = %d, want 2", r.Size())
	}
}

func TestSourceRegistryBoundedEviction(t *testing.T) {
	t.Parallel()
	t0 := time.Now()
	r, err := NewSourceRegistry(10, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Touch three sources, then a fourth: the map must not exceed the
	// cap, and the evicted entry must be the least-recently-touched one.
	if ok, _ := r.Allow("a", t0); !ok {
		t.Fatal("a admitted")
	}
	if ok, _ := r.Allow("b", t0.Add(1*time.Second)); !ok {
		t.Fatal("b admitted")
	}
	if ok, _ := r.Allow("c", t0.Add(2*time.Second)); !ok {
		t.Fatal("c admitted")
	}
	// "a" is now the oldest; a fourth source must evict it.
	if ok, _ := r.Allow("d", t0.Add(3*time.Second)); !ok {
		t.Fatal("d admitted")
	}
	if r.Size() != 3 {
		t.Fatalf("Size = %d, want 3 (bounded)", r.Size())
	}
	// "a" was evicted: its bucket is fresh again (admitted even though it
	// had not been touched since t0).
	if ok, _ := r.Allow("a", t0.Add(4*time.Second)); !ok {
		t.Fatal("evicted source a must be admitted fresh")
	}
}

func TestSourceRegistryRejectsBadConfig(t *testing.T) {
	t.Parallel()
	if _, err := NewSourceRegistry(0, 1, 4); err == nil {
		t.Fatal("zero rate accepted")
	}
	if _, err := NewSourceRegistry(-1, 1, 4); err == nil {
		t.Fatal("negative rate accepted")
	}
}

func TestRegistryCarryOverCarriesUnchangedBucket(t *testing.T) {
	t.Parallel()
	prev := NewRegistry()
	k := &auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}}
	old := prev.For(k)
	// Consume the full burst so a carried bucket is observably drained,
	// unlike a freshly built one: carrying must not re-gift a full burst.
	if ok, _ := old.AllowRate(time.Now()); !ok {
		t.Fatal("fresh bucket must admit")
	}
	if ok, _ := old.AllowRate(time.Now()); !ok {
		t.Fatal("fresh burst-2 bucket must admit twice")
	}

	reg := NewRegistryCarrying(prev)
	got := reg.For(k)
	if got.Bucket == nil {
		t.Fatal("carried key must keep a bucket")
	}
	if got.Bucket != old.Bucket {
		t.Fatal("unchanged limits must carry the same bucket across a reload")
	}
	if ok, _ := got.AllowRate(time.Now()); ok {
		t.Fatal("carried bucket must stay drained (no re-gifted burst)")
	}
}

func TestRegistryCarryOverDerivedBurst(t *testing.T) {
	t.Parallel()
	prev := NewRegistry()
	// Burst omitted: the bucket is built with the derived burst (one
	// second of rate). A reload that also omits burst must still carry
	// it; comparing the raw field would re-gift a fresh burst every
	// reload (FIX-22).
	k := &auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 10, Burst: 0}}
	old := prev.For(k)
	if ok, _ := old.AllowRate(time.Now()); !ok {
		t.Fatal("fresh bucket must admit")
	}

	reg := NewRegistryCarrying(prev)
	got := reg.For(&auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 10, Burst: 0}})
	if got.Bucket != old.Bucket {
		t.Fatal("burst omitted on both sides must still carry the bucket (FIX-22)")
	}
}

func TestRegistryCarryOverRebuildsOnChangedLimits(t *testing.T) {
	t.Parallel()
	// Rate changed: the carried (drained) bucket must be replaced by a
	// fresh one built from the new limits.
	prev := NewRegistry()
	k := &auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}}
	old := prev.For(k)
	old.AllowRate(time.Now())
	old.AllowRate(time.Now()) // fully drained

	reg := NewRegistryCarrying(prev)
	got := reg.For(&auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 5, Burst: 2}})
	if got.Bucket == old.Bucket {
		t.Fatal("changed rate must rebuild the bucket, not carry the old one")
	}
	if ok, _ := got.AllowRate(time.Now()); !ok {
		t.Fatal("rebuilt bucket must be fresh and admit immediately")
	}

	// Explicit burst changed (rate unchanged) must also rebuild.
	prev2 := NewRegistry()
	k2 := &auth.Key{ID: "K2", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}}
	old2 := prev2.For(k2)
	old2.AllowRate(time.Now())
	old2.AllowRate(time.Now())

	reg2 := NewRegistryCarrying(prev2)
	got2 := reg2.For(&auth.Key{ID: "K2", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 5}})
	if got2.Bucket == old2.Bucket {
		t.Fatal("changed burst must rebuild the bucket, not carry the old one")
	}
	if ok, _ := got2.AllowRate(time.Now()); !ok {
		t.Fatal("rebuilt bucket must be fresh and admit immediately")
	}

	// Omitted burst (derived) on one side, explicit on the other: the
	// effective burst differs, so the bucket must rebuild.
	prev3 := NewRegistry()
	k3 := &auth.Key{ID: "K3", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 0}}
	old3 := prev3.For(k3)
	old3.AllowRate(time.Now())

	reg3 := NewRegistryCarrying(prev3)
	got3 := reg3.For(&auth.Key{ID: "K3", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}})
	if got3.Bucket == old3.Bucket {
		t.Fatal("derived vs explicit burst must rebuild the bucket (FIX-22)")
	}
}

func TestRegistryCarryOverAcrossGenerations(t *testing.T) {
	t.Parallel()
	// R3 (FIX-22 eval): repeated SIGHUPs must not accumulate a chain of
	// dead generations. The carried bucket persists, each generation
	// starts lazy (empty) and is snapshotted, and the drain is never
	// re-gifted.
	k := &auth.Key{ID: "K1", Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 2}}
	prev := NewRegistry()
	old := prev.For(k)
	if ok, _ := old.AllowRate(time.Now()); !ok {
		t.Fatal("fresh bucket must admit")
	}
	if ok, _ := old.AllowRate(time.Now()); !ok {
		t.Fatal("fresh burst-2 bucket must admit twice")
	}

	for i := range 3 {
		next := NewRegistryCarrying(prev)
		if next.Size() != 0 {
			t.Fatalf("generation %d starts non-empty (Size=%d), want lazy", i+1, next.Size())
		}
		got := next.For(k)
		if got.Bucket != old.Bucket {
			t.Fatalf("generation %d did not carry the original bucket", i+1)
		}
		prev = next
	}
	if ok, _ := prev.For(k).AllowRate(time.Now()); ok {
		t.Fatal("carried bucket must stay drained across generations")
	}
}

func TestRegistryCarriesThroughIdleGenerations(t *testing.T) {
	key := &auth.Key{ID: "kept", Limits: auth.KeyLimits{
		RequestsPerSecond: 1, Burst: 1, ConcurrentRequests: 1,
	}}
	r := NewRegistry()
	state := r.For(key)
	now := time.Now()
	state.AllowRate(now)
	release, ok := state.AcquireConcurrency()
	if !ok {
		t.Fatal("initial concurrency admission failed")
	}
	defer release()
	for range 3 {
		r = NewRegistryCarrying(r)
	}
	got := r.For(key)
	if ok, _ := got.AllowRate(now); ok {
		t.Fatal("idle reloads reset the rate bucket")
	}
	if release, ok := got.AcquireConcurrency(); ok {
		release()
		t.Fatal("idle reloads lost an active concurrency slot")
	}
}

func TestRegistryRetainPrunesHistoricalKeys(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"kept", "removed"} {
		r.For(&auth.Key{ID: id, Limits: auth.KeyLimits{RequestsPerSecond: 1, Burst: 1, ConcurrentRequests: 1}})
	}
	r = NewRegistryCarrying(NewRegistryCarrying(r))
	r.Retain(func(id string) bool { return id == "kept" })
	if len(r.carry) != 1 || len(r.carryConc) != 1 || r.carry["kept"] == nil || r.carryConc["kept"] == nil {
		t.Fatal("pruning must retain only current keys across idle generations")
	}
}

// A reload must not transiently double a key's concurrency bound. The
// previous generation's in-flight requests hold release closures bound
// to its channel, so rebuilding an unchanged bound would hand out a
// second full set of slots — and key rotation, the common reason to
// reload, changes no limit at all. A changed limit must still apply
// immediately.
func TestConcurrencyBoundCarriesAcrossReloadWhenUnchanged(t *testing.T) {
	key := auth.Key{ID: "k", Name: "n", Enabled: true,
		Limits: auth.KeyLimits{ConcurrentRequests: 2}}

	r1 := NewRegistry()
	ks1 := r1.For(&key)
	relA, okA := ks1.AcquireConcurrency()
	relB, okB := ks1.AcquireConcurrency()
	if !okA || !okB {
		t.Fatal("could not take the two configured slots")
	}
	if _, ok := ks1.AcquireConcurrency(); ok {
		t.Fatal("a third slot was granted past the limit")
	}

	// Reload with the same limit while both slots are still held.
	r2 := NewRegistryCarrying(r1)
	ks2 := r2.For(&key)
	if _, ok := ks2.AcquireConcurrency(); ok {
		t.Fatal("the reload handed out a slot while the previous generation still held both")
	}
	relA()
	if rel, ok := ks2.AcquireConcurrency(); !ok {
		t.Fatal("a slot released by the previous generation was not reusable after reload")
	} else {
		rel()
	}
	relB()

	// A changed limit must not be carried.
	changed := key
	changed.Limits.ConcurrentRequests = 5
	r3 := NewRegistryCarrying(r2)
	ks3 := r3.For(&changed)
	held := 0
	for range 5 {
		if _, ok := ks3.AcquireConcurrency(); ok {
			held++
		}
	}
	if held != 5 {
		t.Fatalf("changed limit granted %d slots, want 5 (it must apply immediately)", held)
	}
}
