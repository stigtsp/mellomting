package proxy

import (
	"sync"
	"time"
)

// affinity is the bounded in-memory Responses API affinity table
// (PLAN §21.1, §21.4).
//
// It maps (API key ID, public response ID) -> (backend, expiry). Records
// are bound to the key that created the response: a different key cannot
// retrieve or cancel it even if it learns the ID (PLAN §21.1). The table
// is cleared on restart by design (PLAN §21.4).

type affKey struct {
	KeyID  string
	RespID string
}

type affVal struct {
	Backend string
	Expiry  time.Time
}

type affinity struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	table map[affKey]affVal
	now   func() time.Time
}

func newAffinity(ttl time.Duration, max int) *affinity {
	if max <= 0 {
		max = 1
	}
	return &affinity{
		ttl:   ttl,
		max:   max,
		table: make(map[affKey]affVal),
		now:   time.Now,
	}
}

// Put records (keyID, respID) -> backend, evicting if the table is full.
func (a *affinity) Put(keyID, respID, backend string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evictLocked(now)
	a.table[affKey{KeyID: keyID, RespID: respID}] = affVal{
		Backend: backend,
		Expiry:  now.Add(a.ttl),
	}
}

// Get resolves the owning backend for (keyID, respID).
func (a *affinity) Get(keyID, respID string) (string, bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.table[affKey{KeyID: keyID, RespID: respID}]
	if !ok || now.After(v.Expiry) {
		if ok {
			delete(a.table, affKey{KeyID: keyID, RespID: respID})
		}
		return "", false
	}
	return v.Backend, true
}

// evictProbeBudget bounds the work of one eviction: when the table is at
// its bound, a Put probes at most this many live entries and evicts the
// oldest of those it saw (FIX-16), keeping eviction O(1)-ish even at the
// MaxAffinityEntries bound instead of two full-table scans per Put.
const evictProbeBudget = 64

// evictLocked drops expired entries; if still at/over the bound it
// removes the oldest entry within a bounded probe window (bounded work,
// PLAN §21.4).
func (a *affinity) evictLocked(now time.Time) {
	if len(a.table) < a.max {
		return
	}
	var oldest affKey
	var oldestT time.Time
	haveOldest := false
	probed := 0
	for k, v := range a.table {
		if now.After(v.Expiry) {
			delete(a.table, k)
			continue
		}
		probed++
		if !haveOldest || v.Expiry.Before(oldestT) {
			oldest, oldestT, haveOldest = k, v.Expiry, true
		}
		if probed >= evictProbeBudget && len(a.table) >= a.max {
			break
		}
	}
	if len(a.table) >= a.max && haveOldest {
		delete(a.table, oldest)
	}
}
