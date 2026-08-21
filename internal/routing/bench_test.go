package routing

import (
	"fmt"
	"testing"

	"mellomting/internal/config"
)

// BenchmarkModelLookup measures the model-name to backend resolution
// across a wide routing table (PLAN §87): the map lookup plus the
// first-select step.
func BenchmarkModelLookup(b *testing.B) {
	backends := map[string]config.Backend{}
	for i := 0; i < 50; i++ {
		backends[fmt.Sprintf("back-%d", i)] = config.Backend{BaseURL: fmt.Sprintf("http://127.0.0.1:800%d", i), UpstreamModel: "Up"}
	}
	models := map[string]config.Model{}
	for i := 0; i < 200; i++ {
		models[fmt.Sprintf("model-%d", i)] = config.Model{
			Type:     "generation",
			Strategy: "single",
			Backends: []config.BackendRef{{Name: fmt.Sprintf("back-%d", i%50), Weight: 1}},
		}
	}
	r, err := New(&config.Config{Backends: backends, Models: models}, func(string) int { return 0 })
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Select(fmt.Sprintf("model-%d", i%200), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBackendRouting measures replica selection for a single
// hot model behind several backends with the round-robin strategy
// (PLAN §87).
func BenchmarkBackendRouting(b *testing.B) {
	backends := map[string]config.Backend{}
	refs := []config.BackendRef{}
	for i := 0; i < 16; i++ {
		name := fmt.Sprintf("back-%d", i)
		backends[name] = config.Backend{BaseURL: fmt.Sprintf("http://127.0.0.1:800%d", i), UpstreamModel: "Up"}
		refs = append(refs, config.BackendRef{Name: name, Weight: 1})
	}
	cfg := &config.Config{
		Backends: backends,
		Models: map[string]config.Model{
			"hot-model": {Type: "generation", Strategy: "round-robin", Backends: refs},
		},
	}
	r, err := New(cfg, func(string) int { return 0 })
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Select("hot-model", nil); err != nil {
			b.Fatal(err)
		}
	}
}
