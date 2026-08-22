// Package routing maps public model names to one eligible backend per
// request and its upstream model name (PLAN §13, §19, §70, §93).
//
// Strategies (PLAN §19): single, round-robin, weighted-round-robin,
// least-inflight, weighted-least-inflight. single/round-robin/WRR are
// deterministic for a given configuration; the least-inflight variants
// pick by live admission load, which changes with concurrent traffic, so
// their selection order is not deterministic across calls. There is no
// latency learning in v1.
//
// Passive health (PLAN §70): a backend that fails at the connection
// level is marked temporarily unavailable with an exponentially growing
// cooldown that resets on the first observed success. The state is
// bounded (one entry per configured backend).
package routing

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"mellomting/internal/config"
)

var (
	// ErrUnknownModel is returned when a public model name is not
	// configured.
	ErrUnknownModel = errors.New("unknown model")
	// ErrNoEligibleBackend is returned when a model has backends but
	// none is selectable right now (all excluded by the caller or all
	// temporarily unavailable per passive health).
	ErrNoEligibleBackend = errors.New("no eligible backend")
)

// supportedStrategies is the router's acceptance set, derived from the
// canonical config.SupportedStrategies list so the config validator and
// the router can never drift (FIX-24b).
var supportedStrategies = func() map[string]bool {
	m := make(map[string]bool, len(config.SupportedStrategies))
	for _, s := range config.SupportedStrategies {
		m[s] = true
	}
	return m
}()

// Passive-health timing (PLAN §70): short base cooldown, exponential
// growth, capped. Deliberately not configuration: the operator controls
// availability through backends, not through internal heuristics.
const (
	healthBaseCooldown = 5 * time.Second
	healthMaxCooldown  = time.Minute
	healthShiftCap     = 16
)

// Target is the routing decision for one request.
type Target struct {
	// PublicModel is the client-visible model name.
	PublicModel string
	// Backend is the configured backend name.
	Backend string
	// Upstream is the model name to send to the backend.
	Upstream string
	// Type is "generation" or "embedding".
	Type string
}

// replica is one configured backend behind a public model.
type replica struct {
	name     string
	upstream string
	weight   int
}

type modelEntry struct {
	typ      string
	strategy string
	replicas []replica
}

// Router maps public models to their backend replicas and applies the
// per-model strategy, the caller's exclusion set, and passive health.
// It is safe for concurrent use.
type Router struct {
	models map[string]modelEntry
	order  []string

	health   *health
	inflight func(backendName string) int

	rrMu sync.Mutex
	rr   map[string]uint64 // per-model round-robin counter

	wrrMu sync.Mutex
	wrr   map[string]*wrrState // per-model smooth weighted round-robin
}

// wrrState is the classic smooth weighted round-robin state indexed by
// replica position within the model.
type wrrState struct {
	current []int
}

// New builds a Router from validated configuration (PLAN §93).
// inflight reports current per-backend load for the *-inflight
// strategies; it may be nil (treated as zero load).
func New(cfg *config.Config, inflight func(string) int) (*Router, error) {
	if len(cfg.Models) == 0 {
		return nil, errors.New("routing: at least one public model is required")
	}
	if inflight == nil {
		inflight = func(string) int { return 0 }
	}
	r := &Router{
		models:   make(map[string]modelEntry, len(cfg.Models)),
		rr:       make(map[string]uint64, len(cfg.Models)),
		wrr:      make(map[string]*wrrState, len(cfg.Models)),
		health:   newHealth(time.Now),
		inflight: inflight,
	}
	names := make([]string, 0, len(cfg.Models))
	for name, m := range cfg.Models {
		if len(m.Backends) == 0 {
			return nil, fmt.Errorf("routing: model %q has no backends", name)
		}
		if !supportedStrategies[m.Strategy] {
			return nil, fmt.Errorf("routing: model %q has unsupported strategy %q", name, m.Strategy)
		}
		reps := make([]replica, 0, len(m.Backends))
		for _, ref := range m.Backends {
			b, ok := cfg.Backends[ref.Name]
			if !ok {
				return nil, fmt.Errorf("routing: model %q references unknown backend %q", name, ref.Name)
			}
			if b.UpstreamModel == "" {
				return nil, fmt.Errorf("routing: backend %q has no upstream_model", ref.Name)
			}
			w := ref.Weight
			if w < 1 {
				w = 1
			}
			reps = append(reps, replica{name: ref.Name, upstream: b.UpstreamModel, weight: w})
		}
		r.models[name] = modelEntry{typ: m.Type, strategy: m.Strategy, replicas: reps}
		names = append(names, name)
	}
	sort.Strings(names)
	r.order = names
	return r, nil
}

// Select picks the backend for one attempt of a request (PLAN §19, §22).
// exclude removes the backends already tried for this request
// (fallback never repeats a tried backend while others remain).
func (r *Router) Select(publicModel string, exclude []string) (Target, error) {
	entry, ok := r.models[publicModel]
	if !ok {
		return Target{}, ErrUnknownModel
	}
	cands := make([]int, 0, len(entry.replicas))
	for i, rep := range entry.replicas {
		if excluded(exclude, rep.name) || r.health.unavailable(rep.name) {
			continue
		}
		cands = append(cands, i)
	}
	if len(cands) == 0 {
		return Target{}, ErrNoEligibleBackend
	}

	var pick int
	switch entry.strategy {
	case "single":
		pick = cands[0]
	case "round-robin":
		r.rrMu.Lock()
		r.rr[publicModel]++
		n := r.rr[publicModel] - 1
		r.rrMu.Unlock()
		pick = cands[int(n)%len(cands)]
	case "weighted-round-robin":
		// nextWRR returns a replica index (drawn from cands); unlike
		// "round-robin" it is not a position within cands, so no outer
		// indexing is applied here.
		pick = r.nextWRR(publicModel, entry, cands)
	case "least-inflight":
		best := cands[0]
		for _, i := range cands[1:] {
			if r.inflight(entry.replicas[i].name) < r.inflight(entry.replicas[best].name) {
				best = i
			}
		}
		pick = best
	case "weighted-least-inflight":
		best := cands[0]
		bestScore := score(entry.replicas[best], r.inflight)
		for _, i := range cands[1:] {
			if s := score(entry.replicas[i], r.inflight); s < bestScore {
				best, bestScore = i, s
			}
		}
		pick = best
	}
	rep := entry.replicas[pick]
	return Target{
		PublicModel: publicModel,
		Backend:     rep.name,
		Upstream:    rep.upstream,
		Type:        entry.typ,
	}, nil
}

// score is the weighted least-inflight score (PLAN §19, approximate):
// (inflight + 1) / weight.
func score(rep replica, inflight func(string) int) float64 {
	return float64(inflight(rep.name)+1) / float64(rep.weight)
}

// nextWRR runs one iteration of the classic smooth weighted round-robin
// (deterministic; ties resolve to the earlier replica position).
func (r *Router) nextWRR(model string, entry modelEntry, cands []int) int {
	r.wrrMu.Lock()
	defer r.wrrMu.Unlock()
	st := r.wrr[model]
	if st == nil {
		st = &wrrState{current: make([]int, len(entry.replicas))}
		r.wrr[model] = st
	}
	for len(st.current) != len(entry.replicas) {
		st = &wrrState{current: make([]int, len(entry.replicas))}
		r.wrr[model] = st
	}
	total := 0
	for _, i := range cands {
		st.current[i] += entry.replicas[i].weight
		total += entry.replicas[i].weight
	}
	best := cands[0]
	for _, i := range cands[1:] {
		if st.current[i] > st.current[best] {
			best = i
		}
	}
	st.current[best] -= total
	return best
}

func excluded(exclude []string, name string) bool {
	for _, e := range exclude {
		if e == name {
			return true
		}
	}
	return false
}

// Has reports whether the public model exists (without saying more).
func (r *Router) Has(publicModel string) bool {
	_, ok := r.models[publicModel]
	return ok
}

// List returns the public model names, sorted.
func (r *Router) List() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// TypeOf returns the model type ("generation" or "embedding").
func (r *Router) TypeOf(publicModel string) string {
	e, ok := r.models[publicModel]
	if !ok {
		return ""
	}
	return e.typ
}

// UpstreamFor returns the upstream model name a backend must serve.
// A backend serves exactly one upstream model (its configuration).
func (r *Router) UpstreamFor(backendName string) (string, bool) {
	for _, name := range r.order {
		for _, rep := range r.models[name].replicas {
			if rep.name == backendName {
				return rep.upstream, true
			}
		}
	}
	return "", false
}

// RecordFailure marks a backend temporarily unavailable after a
// connection-level failure (PLAN §70). Cooldowns grow exponentially
// with consecutive failures and are capped.
func (r *Router) RecordFailure(backendName string) {
	r.health.recordFailure(backendName)
}

// RecordSuccess clears a backend's passive-health state after a
// response was received (PLAN §70).
func (r *Router) RecordSuccess(backendName string) {
	r.health.recordSuccess(backendName)
}

// --- passive health (PLAN §70) ---

type hState struct {
	until  time.Time
	streak int
}

type health struct {
	mu     sync.Mutex
	now    func() time.Time
	base   time.Duration
	max    time.Duration
	states map[string]*hState
}

func newHealth(now func() time.Time) *health {
	return &health{
		now:    now,
		base:   healthBaseCooldown,
		max:    healthMaxCooldown,
		states: make(map[string]*hState),
	}
}

func (h *health) unavailable(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.states[name]
	return ok && s.until.After(h.now())
}

func (h *health) recordFailure(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.states[name]
	if s == nil {
		s = &hState{}
		h.states[name] = s
	}
	s.streak++
	shift := s.streak - 1
	if shift > healthShiftCap {
		shift = healthShiftCap
	}
	cooldown := h.base << shift
	if cooldown > h.max {
		cooldown = h.max
	}
	s.until = h.now().Add(cooldown)
}

func (h *health) recordSuccess(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, name)
}
