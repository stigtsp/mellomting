package limiter

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkBucketAllow measures the per-request token-bucket check for
// a high-rate key (PLAN §87): the hot path of per-key RPS limiting.
func BenchmarkBucketAllow(b *testing.B) {
	bk, err := NewBucket(1_000_000, 1_000_000)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = bk.Allow(time.Now())
	}
}

// BenchmarkSourceRegistryAllowFull measures Allow on a source registry at
// its cap (FIX-17): each new source must evict within a bounded probe
// window rather than scan the whole map, so cost stays constant at the
// DefaultPreauthSources bound.
func BenchmarkSourceRegistryAllowFull(b *testing.B) {
	r, err := NewSourceRegistry(1_000_000, 1, 4096)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	for i := range 4096 {
		ok, _ := r.Allow(fmt.Sprintf("10.0.0.%d", i), now)
		if !ok {
			b.Fatal("pre-fill request rejected")
		}
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		_, _ = r.Allow(fmt.Sprintf("10.0.1.%d", i%1024), now)
	}
}
