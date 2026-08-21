package proxy

import "sync/atomic"

// budget is a process-wide weighted byte budget for buffered inference
// requests (PLAN §12.1).
//
// A request that must be inspected (shallow parse + rewrite) peaks at
// well above its raw body size: decoding into a map[string]json.RawMessage
// and re-encoding costs ~13× the body for adversarial shapes (a body of
// many short keys), so a request of n body bytes reserves budgetWeight*n
// (T-X12). Requests that cannot acquire the budget quickly receive a
// bounded overload error instead of waiting (PLAN §12.1).

// budgetWeight is the multiplier applied to a request body size when
// reserving the byte budget (T-X12). The measured decode+re-encode peak
// is ~13× body for many-short-key shapes; 16 is a conservative power of
// two that keeps the aggregate bound (MaxBufferedRequestBytes ×
// MaxInflightRequests) meaningful.
const budgetWeight = 16

type budget struct {
	total int64
	used  atomic.Int64
}

func newBudget(total int64) *budget {
	return &budget{total: total}
}

// Acquire reserves weight (budgetWeight * bodyBytes) or reports overload.
func (b *budget) Acquire(bodyBytes int64) bool {
	if bodyBytes > b.total/budgetWeight {
		return false
	}
	weight := bodyBytes * budgetWeight
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
	b.used.Add(-bodyBytes * budgetWeight)
}
