package proxy

import "sync/atomic"

// budget is a process-wide weighted byte budget for buffered inference
// requests (PLAN §12.1).
//
// A request that must be inspected (shallow parse + rewrite) peaks at
// roughly twice its body size (original buffer + re-encoded JSON), so a
// request of n body bytes reserves 2n. Requests that cannot acquire the
// budget quickly receive a bounded overload error instead of waiting
// (PLAN §12.1).

type budget struct {
	total int64
	used  atomic.Int64
}

func newBudget(total int64) *budget {
	return &budget{total: total}
}

// Acquire reserves weight (2*n body bytes) or reports overload.
func (b *budget) Acquire(bodyBytes int64) bool {
	if bodyBytes > b.total/2 {
		return false
	}
	weight := bodyBytes * 2
	for {
		cur := b.used.Load()
		if cur+weight > b.total {
			return false
		}
		if b.used.CompareAndSwap(cur, cur+weight) {
			return true
		}
	}
}

// Release returns the reservation.
func (b *budget) Release(bodyBytes int64) {
	b.used.Add(-bodyBytes * 2)
}
