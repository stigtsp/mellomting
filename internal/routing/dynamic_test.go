package routing

import (
	"errors"
	"slices"
	"testing"

	"mellomting/internal/config"
)

func agentsOnlyRouter(t *testing.T) *Router {
	t.Helper()
	r, err := New(&config.Config{Agents: config.Agents{Enabled: true}}, nil)
	if err != nil {
		t.Fatalf("an agents-only router needs no static model: %v", err)
	}
	return r
}

func TestDynamicModelsLifecycle(t *testing.T) {
	r := agentsOnlyRouter(t)
	if err := r.AddDynamic("lab/m1", "agent-a", "m1", 0, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddDynamic("lab/m1", "agent-b", "m1", 0, "k1"); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same pair is a no-op, not a second replica.
	_ = r.AddDynamic("lab/m1", "agent-a", "m1", 0, "k1")
	if err := r.AddDynamic("lab/m2", "agent-a", "m2", 0, "k1"); err != nil {
		t.Fatal(err)
	}
	if got := r.List(); !slices.Equal(got, []string{"lab/m1", "lab/m2"}) {
		t.Fatalf("List = %v", got)
	}
	if r.TypeOf("lab/m1") != "" {
		t.Fatal("attached models have no type: the server decides which endpoints serve them")
	}
	if up, ok := r.UpstreamForModel("lab/m2", "agent-a"); !ok || up != "m2" {
		t.Fatalf("UpstreamForModel = %q %v", up, ok)
	}
	seen := map[string]bool{}
	for range 4 {
		tg, err := r.Select("lab/m1", nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[tg.Backend] = true
	}
	if tg, err := r.Select("lab/m1", []string{"agent-a"}); err != nil || tg.Backend != "agent-b" {
		t.Fatalf("exclusion: %v %v", tg, err)
	}

	r.RemoveDynamic("agent-a")
	if r.Has("lab/m2") {
		t.Fatal("a model whose only replica detached must disappear")
	}
	if tg, err := r.Select("lab/m1", nil); err != nil || tg.Backend != "agent-b" {
		t.Fatalf("remaining replica: %v %v", tg, err)
	}
	r.RemoveDynamic("agent-b")
	if len(r.List()) != 0 || r.Has("lab/m1") {
		t.Fatal("all attached models must be gone")
	}
	if _, err := r.Select("lab/m1", nil); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Select after detach: %v", err)
	}
}

func TestDynamicCannotShadowStatic(t *testing.T) {
	cfg := &config.Config{
		Agents:   config.Agents{Enabled: true},
		Backends: map[string]config.Backend{"b": {UpstreamModel: "m"}},
		Models:   map[string]config.Model{"lab/m1": {Type: "generation", Strategy: "single", Backends: []config.BackendRef{{Name: "b"}}}},
	}
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddDynamic("lab/m1", "agent-a", "m1", 0, "k1"); !errors.Is(err, ErrStaticModel) {
		t.Fatalf("got %v, want ErrStaticModel", err)
	}
	if tg, _ := r.Select("lab/m1", nil); tg.Backend != "b" {
		t.Fatal("the static model must keep routing to its backend")
	}
}

func TestNoModelsWithoutAgents(t *testing.T) {
	if _, err := New(&config.Config{}, nil); err == nil {
		t.Fatal("a router with no models and agents disabled must be refused")
	}
}
