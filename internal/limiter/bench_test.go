package limiter

import (
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bk.Allow(time.Now())
	}
}
