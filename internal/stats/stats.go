// Package stats keeps the last few minutes of traffic, per second, for
// `mellomting top` (PLAN §43.2): completed requests, refusals and the
// limit behind each, bytes, tokens, latency, and once-a-second gauges
// of in-flight work and backend load.
//
// It exists because a request a limit refuses never enters the
// in-flight registry, so a view of live requests alone shows a calm
// table while clients are being turned away.
//
// Memory is bounded by construction: a fixed ring of one-second buckets
// per series, at most MaxSeries series per dimension (names beyond fold
// into OtherID), and series idle for the whole window are dropped. Every
// method is safe on a nil *Store, so a daemon without an admin socket
// runs the same call sites with nothing recorded.
package stats

import (
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Window is how far back the store remembers.
	Window = 300 * time.Second
	slots  = int(Window / time.Second)
	// MaxSeries bounds the series of each dimension (keys, models,
	// servers). Model names arrive from agents and server names from
	// attachments, which reconnect under new names, so neither is
	// bounded by configuration.
	MaxSeries = 64

	// MaxID bounds a series ID or name in bytes. Callers pass routed
	// names; this is the backstop if one passes a client's string.
	MaxID = 256
	// OtherID names the series that collects whatever is past
	// MaxSeries.
	OtherID = "(other)"
	// maxClasses bounds the error classes kept per series. The classes
	// are the proxy's own fixed set; the bound is a backstop.
	maxClasses = 32
)

// Refusal is the limit that turned a request away.
type Refusal string

// Refusals, in the order the export lists them.
const (
	GlobalInflight Refusal = "global_inflight"
	SourceRate     Refusal = "source_rate"
	GlobalRate     Refusal = "global_rate"
	Auth           Refusal = "auth"
	KeyRate        Refusal = "key_rate"
	KeyConcurrency Refusal = "key_concurrency"
	Quota          Refusal = "quota"
	BackendQueue   Refusal = "backend_queue"
	Draining       Refusal = "draining"
	// BodyBudget is the memory shared by request bodies being read
	// (max_buffered_request_bytes × max_inflight_requests).
	BodyBudget Refusal = "body_budget"
)

var refusals = [...]Refusal{GlobalInflight, SourceRate, GlobalRate, Auth, KeyRate, KeyConcurrency, Quota, BackendQueue, Draining, BodyBudget}

func refusalIndex(r Refusal) int {
	for i, x := range refusals {
		if x == r {
			return i
		}
	}
	return -1
}

// terminalRefusal maps the error classes with which the proxy ends a
// request because of a limit, rather than a failure, to the limit.
func terminalRefusal(class string) (Refusal, bool) {
	switch class {
	case "queue_full", "queue_timeout":
		return BackendQueue, true
	case "token_quota":
		return Quota, true
	case "overload":
		return Draining, true
	case "body_budget":
		return BodyBudget, true
	}
	return "", false
}

// Sample is one finished request, as the access log records it. Server
// is the inference server (or agent attachment), not the per-model
// backend name. Tokens are what accounting charged.
type Sample struct {
	KeyID, KeyName, Model, Server string
	Status                        int
	ErrorClass                    string
	BytesIn, BytesOut             int64
	TokensIn, TokensOut           int64
	TTFB, Duration                time.Duration
}

// Gauges is one second's view of live load, taken off the request path.
type Gauges struct {
	GlobalInflight int
	Keys           map[string]KeyGauge    // by key ID
	Servers        map[string]ServerGauge // by server name
}

// KeyGauge is a key's live state. AtLimit is whether its in-flight count
// was at its concurrent_requests bound when sampled.
type KeyGauge struct {
	Name     string
	Inflight int
	AtLimit  bool
}

// ServerGauge is a server's live state: the edge's admission, and the
// server's own report when it publishes metrics.
type ServerGauge struct {
	Admitted, Queued int
	HasMetrics       bool
	Running, Waiting int
	KVPercent        float64
}

// histEdges are the duration histogram's upper bounds in milliseconds;
// the last bucket is open.
var histEdges = [...]int64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000}

const histBuckets = len(histEdges) + 1

func histIndex(ms int64) int {
	for i, e := range histEdges {
		if ms <= e {
			return i
		}
	}
	return len(histEdges)
}

type bucket struct {
	sec               int64 // unix second this slot holds; stale otherwise
	ok, c4xx, c5xx    uint32
	refused           uint32
	refusals          [len(refusals)]uint32
	bytesIn, bytesOut int64
	tokIn, tokOut     int64
	ttfbSum           int64 // ms
	ttfbN             uint32
	durSum            int64 // ms
	hist              [histBuckets]uint32
	// gauges
	gauged           bool
	inflight         uint32
	atLimit          bool
	admitted, queued uint32
	metrics          bool
	running, waiting uint32
	kv               float32
}

type classRing struct {
	secs [slots]int64
	n    [slots]uint32
}

type series struct {
	id, name string
	last     int64 // newest second with data
	ring     [slots]bucket
	errors   map[string]*classRing
}

// Store holds the window. It is safe for concurrent use.
type Store struct {
	clock  func() time.Time
	mu     sync.Mutex
	global series
	dims   [3]map[string]*series // key, model, server
}

var dimNames = [3]string{"key", "model", "server"}

const (
	dimKey = iota
	dimModel
	dimServer
)

// New builds an empty store. clock is time.Now when nil.
func New(clock func() time.Time) *Store {
	if clock == nil {
		clock = time.Now
	}
	s := &Store{clock: clock}
	for i := range s.dims {
		s.dims[i] = make(map[string]*series)
	}
	return s
}

// slot returns the bucket for sec, resetting it if it held an older
// second.
func (se *series) slot(sec int64) *bucket {
	b := &se.ring[sec%int64(slots)]
	if b.sec != sec {
		*b = bucket{sec: sec}
	}
	if sec > se.last {
		se.last = sec
	}
	return b
}

// lookup finds or creates a series; past MaxSeries it returns the
// dimension's OtherID series. An empty id records nothing.
func (s *Store) lookup(dim int, id, name string) *series {
	if id == "" {
		return nil
	}
	id, name = clip(id), clip(name)
	m := s.dims[dim]
	if se, ok := m[id]; ok {
		if name != "" && se.name != name {
			se.name = name
		}
		return se
	}
	if len(m) >= MaxSeries {
		id, name = OtherID, OtherID
		if se, ok := m[id]; ok {
			return se
		}
	}
	se := &series{id: id, name: name}
	m[id] = se
	return se
}

// clip cuts s to MaxID bytes on a rune boundary. Unchanged strings are
// returned as is, so the common path does not allocate.
func clip(s string) string {
	if len(s) <= MaxID {
		return s
	}
	n := MaxID
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (se *series) addError(class string, sec int64) {
	if class == "" {
		return
	}
	if se.errors == nil {
		se.errors = make(map[string]*classRing)
	}
	r, ok := se.errors[class]
	if !ok {
		if len(se.errors) >= maxClasses {
			class = OtherID
			r = se.errors[class]
		}
		if r == nil {
			r = &classRing{}
			se.errors[class] = r
		}
	}
	i := sec % int64(slots)
	if r.secs[i] != sec {
		r.secs[i], r.n[i] = sec, 0
	}
	r.n[i]++
}

// Record counts a finished request. A request a limit ended (queue
// full or timed out, quota, draining) counts as a refusal, not as an
// error or a completed request.
func (s *Store) Record(smp Sample) {
	if s == nil {
		return
	}
	sec := s.clock().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := [4]*series{&s.global,
		s.lookup(dimKey, smp.KeyID, smp.KeyName),
		s.lookup(dimModel, smp.Model, smp.Model),
		s.lookup(dimServer, smp.Server, smp.Server)}
	if r, ok := terminalRefusal(smp.ErrorClass); ok && smp.Status >= 400 {
		i := refusalIndex(r)
		for _, se := range targets {
			if se != nil {
				b := se.slot(sec)
				b.refused++
				b.refusals[i]++
			}
		}
		return
	}
	ttfb, dur := smp.TTFB.Milliseconds(), smp.Duration.Milliseconds()
	for _, se := range targets {
		if se == nil {
			continue
		}
		b := se.slot(sec)
		switch {
		case smp.Status >= 500:
			b.c5xx++
		case smp.Status >= 400:
			b.c4xx++
		default:
			b.ok++
		}
		if smp.Status >= 400 {
			se.addError(smp.ErrorClass, sec)
		}
		b.bytesIn += smp.BytesIn
		b.bytesOut += smp.BytesOut
		b.tokIn += smp.TokensIn
		b.tokOut += smp.TokensOut
		if smp.TTFB > 0 {
			b.ttfbSum += ttfb
			b.ttfbN++
		}
		b.durSum += dur
		b.hist[histIndex(dur)]++
	}
}

// Refuse counts a request a limit turned away before the proxy saw it.
// Empty identifiers are unattributed (an unauthenticated refusal).
func (s *Store) Refuse(r Refusal, keyID, keyName, model, server string) {
	if s == nil {
		return
	}
	i := refusalIndex(r)
	if i < 0 {
		return
	}
	sec := s.clock().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, se := range [4]*series{&s.global,
		s.lookup(dimKey, keyID, keyName),
		s.lookup(dimModel, model, model),
		s.lookup(dimServer, server, server)} {
		if se != nil {
			b := se.slot(sec)
			b.refused++
			b.refusals[i]++
		}
	}
}

// Sample records this second's gauges, and drops series idle for the
// whole window. The daemon calls it once a second.
func (s *Store) Sample(g Gauges) {
	if s == nil {
		return
	}
	sec := s.clock().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.global.slot(sec)
	b.gauged, b.inflight = true, clampU32(g.GlobalInflight)
	for id, kg := range g.Keys {
		if se := s.lookup(dimKey, id, kg.Name); se != nil {
			b := se.slot(sec)
			b.gauged = true
			// Folded keys share a slot: keep the peak.
			b.inflight = max(b.inflight, clampU32(kg.Inflight))
			b.atLimit = b.atLimit || kg.AtLimit
		}
	}
	for name, sg := range g.Servers {
		if se := s.lookup(dimServer, name, name); se != nil {
			b := se.slot(sec)
			b.gauged = true
			b.admitted, b.queued = clampU32(sg.Admitted), clampU32(sg.Queued)
			if sg.HasMetrics {
				b.metrics = true
				b.running, b.waiting = clampU32(sg.Running), clampU32(sg.Waiting)
				b.kv = float32(sg.KVPercent)
			}
		}
	}
	s.pruneLocked(sec)
}

func (s *Store) pruneLocked(now int64) {
	for _, m := range s.dims {
		for id, se := range m {
			if se.last <= now-int64(slots) {
				delete(m, id)
			}
		}
	}
}

func clampU32(n int) uint32 {
	if n < 0 {
		return 0
	}
	if n > 1<<31 {
		return 1 << 31
	}
	return uint32(n)
}
