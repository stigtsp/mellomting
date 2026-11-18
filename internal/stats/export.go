package stats

import (
	"cmp"
	"encoding/json"
	"slices"
)

// BucketExport is one second of one series. Zero fields are omitted, so
// a quiet second costs a few bytes.
type BucketExport struct {
	Seq       int64                  `json:"seq"`
	OK        uint32                 `json:"ok,omitempty"`
	C4xx      uint32                 `json:"c4xx,omitempty"`
	C5xx      uint32                 `json:"c5xx,omitempty"`
	Refused   uint32                 `json:"refused,omitempty"`
	BytesIn   int64                  `json:"bin,omitempty"`
	BytesOut  int64                  `json:"bout,omitempty"`
	TokensIn  int64                  `json:"tin,omitempty"`
	TokensOut int64                  `json:"tout,omitempty"`
	TTFBSumMs int64                  `json:"ttfb_ms_sum,omitempty"`
	TTFBN     uint32                 `json:"ttfb_n,omitempty"`
	DurSumMs  int64                  `json:"dur_ms_sum,omitempty"`
	Hist      *[histBuckets]uint32   `json:"hist,omitempty"`
	Gauged    bool                   `json:"gauged,omitempty"`
	Inflight  uint32                 `json:"inflight,omitempty"`
	AtLimit   bool                   `json:"at_limit,omitempty"`
	Admitted  uint32                 `json:"admitted,omitempty"`
	Queued    uint32                 `json:"queued,omitempty"`
	Metrics   bool                   `json:"metrics,omitempty"`
	Running   uint32                 `json:"running,omitempty"`
	Waiting   uint32                 `json:"waiting,omitempty"`
	KV        float32                `json:"kv,omitempty"`
	Refusals  *[len(refusals)]uint32 `json:"refusals,omitempty"`
}

// SeriesExport is one series: its buckets newer than the caller's seq,
// and totals over the whole window. Buckets is null when the reply was
// truncated and this series lost them; the totals remain.
type SeriesExport struct {
	Dim      string           `json:"dim"`
	ID       string           `json:"id"`
	Name     string           `json:"name,omitempty"`
	Requests int64            `json:"requests"`
	Buckets  []BucketExport   `json:"buckets"`
	Errors   map[string]int64 `json:"errors,omitempty"`
	Refusals map[string]int64 `json:"refusals,omitempty"`
}

// Export is the store as `GET /stats` serves it. Seq is the newest
// complete second; a client passes it back as since to get only what
// is newer. The second still being filled is never exported, so no
// client keeps a partial count.
type Export struct {
	Seq          int64          `json:"seq"`
	BucketMillis int64          `json:"bucket_ms"`
	HistEdgesMs  []int64        `json:"hist_edges_ms"`
	RefusalOrder []Refusal      `json:"refusal_order"`
	Truncated    bool           `json:"truncated,omitempty"`
	Global       SeriesExport   `json:"global"`
	Series       []SeriesExport `json:"series"`
}

// Since exports buckets newer than seq. When the reply would pass
// maxBytes, the least busy series give up their buckets first; the
// global series never does.
func (s *Store) Since(seq int64, maxBytes int) Export {
	if s == nil {
		return Export{}
	}
	now := s.clock().Unix()
	s.mu.Lock()
	s.pruneLocked(now)
	e := Export{
		Seq:          now - 1,
		BucketMillis: 1000,
		HistEdgesMs:  histEdges[:],
		RefusalOrder: refusals[:],
		Global:       s.global.export("global", seq, now, true),
	}
	for d, m := range s.dims {
		for _, se := range m {
			e.Series = append(e.Series, se.export(dimNames[d], seq, now, false))
		}
	}
	slices.SortFunc(e.Series, func(a, b SeriesExport) int {
		if c := cmp.Compare(a.Dim, b.Dim); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	// The export holds copies; sizing it marshals, which is too slow to
	// do while every request waits on the lock.
	s.mu.Unlock()
	e.truncate(maxBytes)
	return e
}

// testHookTruncate runs as truncate starts, for tests.
var testHookTruncate func()

func (e *Export) truncate(maxBytes int) {
	if testHookTruncate != nil {
		testHookTruncate()
	}
	size := func(v any) int {
		b, _ := json.Marshal(v)
		return len(b)
	}
	total := size(e)
	if total <= maxBytes {
		return
	}
	order := make([]int, len(e.Series))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(e.Series[a].Requests, e.Series[b].Requests)
	})
	for _, i := range order {
		if total <= maxBytes {
			break
		}
		if e.Series[i].Buckets == nil {
			continue
		}
		total -= size(e.Series[i].Buckets) - len("null")
		e.Series[i].Buckets = nil
		e.Truncated = true
	}
}

func live(sec, now int64) bool { return sec > now-int64(slots) && sec <= now }

func (se *series) export(dim string, seq, now int64, global bool) SeriesExport {
	out := SeriesExport{Dim: dim, ID: se.id, Name: se.name, Buckets: []BucketExport{}}
	if global {
		out.ID, out.Name = "", ""
	}
	var refs [len(refusals)]int64
	// Oldest first: the ring index of now+1 is the oldest slot.
	for k := 1; k <= slots; k++ {
		b := &se.ring[(now+int64(k))%int64(slots)]
		if !live(b.sec, now) {
			continue
		}
		out.Requests += int64(b.ok + b.c4xx + b.c5xx + b.refused)
		for i, n := range b.refusals {
			refs[i] += int64(n)
		}
		if b.sec <= seq || b.sec >= now {
			continue
		}
		be := BucketExport{
			Seq: b.sec, OK: b.ok, C4xx: b.c4xx, C5xx: b.c5xx, Refused: b.refused,
			BytesIn: b.bytesIn, BytesOut: b.bytesOut, TokensIn: b.tokIn, TokensOut: b.tokOut,
			TTFBSumMs: b.ttfbSum, TTFBN: b.ttfbN, DurSumMs: b.durSum,
			Gauged: b.gauged, Inflight: b.inflight, AtLimit: b.atLimit,
			Admitted: b.admitted, Queued: b.queued, Metrics: b.metrics,
			Running: b.running, Waiting: b.waiting, KV: b.kv,
		}
		if b.ok+b.c4xx+b.c5xx > 0 {
			h := b.hist
			be.Hist = &h
		}
		if global && b.refused > 0 {
			r := b.refusals
			be.Refusals = &r
		}
		out.Buckets = append(out.Buckets, be)
	}
	for i, n := range refs {
		if n > 0 {
			if out.Refusals == nil {
				out.Refusals = make(map[string]int64)
			}
			out.Refusals[string(refusals[i])] = n
		}
	}
	for class, r := range se.errors {
		var n int64
		for i, sec := range r.secs {
			if live(sec, now) {
				n += int64(r.n[i])
			}
		}
		if n > 0 {
			if out.Errors == nil {
				out.Errors = make(map[string]int64)
			}
			out.Errors[class] = n
		}
	}
	return out
}
