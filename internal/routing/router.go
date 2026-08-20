// Package routing maps public model names to exactly one backend and its
// upstream model name (PLAN §13, §19, §92).
//
// Phase 1 routes each public model deterministically to its first
// configured backend; the multi-replica strategies listed in PLAN §19
// (round-robin, least-inflight, ...) are selected here but resolve to the
// first replica until the resilience phase lands, which keeps behaviour
// deterministic and testable (PLAN §19: "keep the implementation
// deterministic and testable").
package routing

import (
	"errors"
	"fmt"
	"sort"

	"mellomting/internal/config"
)

// ErrUnknownModel is returned when a public model name is not configured.
var ErrUnknownModel = errors.New("unknown model")

// Supported strategies (PLAN §19). All resolve deterministically in Phase 1.
var supportedStrategies = map[string]bool{
	"single":                  true,
	"round-robin":             true,
	"weighted-round-robin":    true,
	"least-inflight":          true,
	"weighted-least-inflight": true,
}

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

// Router is the immutable public-model table.
type Router struct {
	models map[string]Target
	order  []string
}

// New builds a Router from validated configuration (PLAN §92: one backend
// per public model initially).
func New(cfg *config.Config) (*Router, error) {
	if len(cfg.Models) == 0 {
		return nil, errors.New("routing: at least one public model is required")
	}
	r := &Router{models: make(map[string]Target, len(cfg.Models))}
	names := make([]string, 0, len(cfg.Models))
	for name, m := range cfg.Models {
		if len(m.Backends) == 0 {
			return nil, fmt.Errorf("routing: model %q has no backends", name)
		}
		if !supportedStrategies[m.Strategy] {
			return nil, fmt.Errorf("routing: model %q has unsupported strategy %q", name, m.Strategy)
		}
		// Phase 1: the first configured backend (deterministic).
		backendName := m.Backends[0].Name
		b, ok := cfg.Backends[backendName]
		if !ok {
			return nil, fmt.Errorf("routing: model %q references unknown backend %q", name, backendName)
		}
		if b.UpstreamModel == "" {
			return nil, fmt.Errorf("routing: backend %q has no upstream_model", backendName)
		}
		r.models[name] = Target{
			PublicModel: name,
			Backend:     backendName,
			Upstream:    b.UpstreamModel,
			Type:        m.Type,
		}
		names = append(names, name)
	}
	sort.Strings(names)
	r.order = names
	return r, nil
}

// Resolve returns the routing target for a public model (PLAN §13: the
// name must match exactly; no provider/model syntax is accepted).
func (r *Router) Resolve(publicModel string) (Target, error) {
	t, ok := r.models[publicModel]
	if !ok {
		return Target{}, ErrUnknownModel
	}
	return t, nil
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
