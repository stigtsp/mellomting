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
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 1000; i++ {
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
	for i := 0; i < 50; i++ {
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
