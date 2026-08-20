package routing

import (
	"testing"
	"time"

	"mellomting/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Backends: map[string]config.Backend{
			"back-a": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "Up/A"},
			"back-b": {BaseURL: "http://127.0.0.1:8002", UpstreamModel: "Up/B"},
			"back-c": {BaseURL: "http://127.0.0.1:8003", UpstreamModel: "Up/C"},
		},
		Models: map[string]config.Model{
			"qwen-coder": {
				Type:     "generation",
				Strategy: "least-inflight",
				Backends: []config.BackendRef{
					{Name: "back-a", Weight: 1},
					{Name: "back-b", Weight: 1},
				},
			},
			"embed-small": {
				Type:     "embedding",
				Strategy: "single",
				Backends: []config.BackendRef{{Name: "back-b", Weight: 1}},
			},
		},
	}
}

func TestSelectLeastInflight(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig(), func(name string) int {
		switch name {
		case "back-a":
			return 7
		case "back-b":
			return 2
		}
		return 0
	})
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.Select("qwen-coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Backend != "back-b" {
		t.Fatalf("least-inflight picked %q, want back-b (2 < 7)", t1.Backend)
	}
	if t1.Upstream != "Up/B" || t1.PublicModel != "qwen-coder" || t1.Type != "generation" {
		t.Fatalf("target = %+v", t1)
	}

	// Ties resolve to the earlier configuration position.
	r2, _ := New(testConfig(), func(string) int { return 3 })
	tie, _ := r2.Select("qwen-coder", nil)
	if tie.Backend != "back-a" {
		t.Fatalf("tie-break = %q, want back-a", tie.Backend)
	}
}

func TestSelectRoundRobin(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Backends: map[string]config.Backend{
			"back-a": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "Up/A"},
			"back-b": {BaseURL: "http://127.0.0.1:8002", UpstreamModel: "Up/B"},
		},
		Models: map[string]config.Model{
			"m": {Type: "generation", Strategy: "round-robin",
				Backends: []config.BackendRef{{Name: "back-a"}, {Name: "back-b"}}},
		},
	}
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"back-a", "back-b", "back-a", "back-b"}
	for i, w := range want {
		t1, err := r.Select("m", nil)
		if err != nil {
			t.Fatal(err)
		}
		if t1.Backend != w {
			t.Fatalf("pick %d = %q, want %q", i, t1.Backend, w)
		}
	}
}

func TestSelectWeightedRoundRobin(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Backends: map[string]config.Backend{
			"back-a": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "Up/A"},
			"back-b": {BaseURL: "http://127.0.0.1:8002", UpstreamModel: "Up/B"},
		},
		Models: map[string]config.Model{
			"m": {Type: "generation", Strategy: "weighted-round-robin",
				Backends: []config.BackendRef{{Name: "back-a", Weight: 3}, {Name: "back-b", Weight: 1}}},
		},
	}
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for i := 0; i < 40; i++ {
		t1, err := r.Select("m", nil)
		if err != nil {
			t.Fatal(err)
		}
		count[t1.Backend]++
	}
	if count["back-a"] != 30 || count["back-b"] != 10 {
		t.Fatalf("distribution = %v, want 30/10", count)
	}
	// Smooth WRR spreads the lighter backend in between heavier ones:
	// over the first 4 picks it must appear at least once.
	first4 := map[string]int{}
	for i := 0; i < 4; i++ {
		t1, err := r.Select("m", nil)
		if err != nil {
			t.Fatal(err)
		}
		first4[t1.Backend]++
	}
	if first4["back-b"] == 0 {
		t.Fatalf("lighter backend missing in first 4 picks: %v", first4)
	}
}

func TestSelectWeightedLeastInflight(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Backends: map[string]config.Backend{
			"back-a": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "Up/A"},
			"back-b": {BaseURL: "http://127.0.0.1:8002", UpstreamModel: "Up/B"},
		},
		Models: map[string]config.Model{
			"m": {Type: "generation", Strategy: "weighted-least-inflight",
				Backends: []config.BackendRef{{Name: "back-a", Weight: 1}, {Name: "back-b", Weight: 4}}},
		},
	}
	// inflight a=0 (score 1/1=1.0), b=3 (score 4/4=1.0): tie -> config
	// order -> back-a.
	r, err := New(cfg, func(name string) int {
		if name == "back-b" {
			return 3
		}
		return 0
	})
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.Select("m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Backend != "back-a" {
		t.Fatalf("tie score = %q, want back-a", t1.Backend)
	}
	// inflight a=0 (1.0), b=0 (0.25): back-b wins.
	r2, _ := New(cfg, func(string) int { return 0 })
	t2, _ := r2.Select("m", nil)
	if t2.Backend != "back-b" {
		t.Fatalf("weighted pick = %q, want back-b", t2.Backend)
	}
}

func TestSelectExcludeAndNone(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.Select("qwen-coder", []string{"back-a"})
	if err != nil {
		t.Fatal(err)
	}
	if t1.Backend != "back-b" {
		t.Fatalf("exclude back-a -> %q, want back-b", t1.Backend)
	}
	if _, err := r.Select("qwen-coder", []string{"back-a", "back-b"}); err != ErrNoEligibleBackend {
		t.Fatalf("all excluded: err = %v", err)
	}
}

func TestPassiveHealth(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	h := newHealth(func() time.Time { return now })

	if h.unavailable("back-a") {
		t.Fatal("fresh backend marked unavailable")
	}
	h.recordFailure("back-a")
	if !h.unavailable("back-a") {
		t.Fatal("failure did not cool down the backend")
	}
	// Cooldown grows with consecutive failures and is capped.
	first := h.states["back-a"].until
	now = now.Add(3 * time.Second) // still inside the first (5s) cooldown
	if !h.unavailable("back-a") {
		t.Fatal("backend admitted inside its own cooldown")
	}
	h.recordFailure("back-a") // streak 2: 10s cooldown
	second := h.states["back-a"].until
	if !second.After(first.Add(healthBaseCooldown)) {
		t.Fatalf("cooldown did not grow: first=%v second=%v", first, second)
	}
	now = now.Add(healthMaxCooldown + 2*time.Second) // well past any cap
	if h.unavailable("back-a") {
		t.Fatal("cooldown did not expire (cap exceeded)")
	}

	// Success clears the state.
	now = time.Unix(1000, 0)
	h.recordFailure("back-b")
	h.recordSuccess("back-b")
	if h.unavailable("back-b") {
		t.Fatal("success did not clear the state")
	}
}

func TestHealthExcludesFromSelection(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	r.health = newHealth(func() time.Time { return now })

	t1, err := r.Select("qwen-coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Backend != "back-a" {
		t.Fatalf("baseline = %q, want back-a", t1.Backend)
	}

	r.RecordFailure("back-a")
	t2, err := r.Select("qwen-coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Backend != "back-b" {
		t.Fatalf("after failure = %q, want back-b", t2.Backend)
	}

	// Both cooling down: nothing selectable.
	r.RecordFailure("back-b")
	if _, err := r.Select("qwen-coder", nil); err != ErrNoEligibleBackend {
		t.Fatalf("all cooling: err = %v", err)
	}

	// Success re-admits.
	r.RecordSuccess("back-a")
	t3, _ := r.Select("qwen-coder", []string{"back-b"})
	if t3.Backend != "back-a" {
		t.Fatalf("after success = %q, want back-a", t3.Backend)
	}
}

func TestSelectUnknownAndAccessors(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Select("nope", nil); err != ErrUnknownModel {
		t.Fatalf("err = %v", err)
	}
	// provider/model syntax must not implicitly resolve.
	if _, err := r.Select("provider/qwen-coder", nil); err != ErrUnknownModel {
		t.Fatalf("provider/model syntax accepted: %v", err)
	}
	if r.Has("qwen-coder") != true || r.Has("nope") != false {
		t.Fatal("Has wrong")
	}
	if got := r.List(); len(got) != 2 || got[0] != "embed-small" || got[1] != "qwen-coder" {
		t.Fatalf("List = %v", got)
	}
	if got := r.BackendsFor("qwen-coder"); len(got) != 2 || got[0] != "back-a" || got[1] != "back-b" {
		t.Fatalf("BackendsFor = %v", got)
	}
	if r.TypeOf("embed-small") != "embedding" {
		t.Fatalf("TypeOf = %q", r.TypeOf("embed-small"))
	}
	if up, ok := r.UpstreamFor("back-a"); !ok || up != "Up/A" {
		t.Fatalf("UpstreamFor = %q %v", up, ok)
	}
	if _, ok := r.UpstreamFor("ghost"); ok {
		t.Fatal("UpstreamFor ghost should fail")
	}
}

func TestNewRejectsBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*config.Config)
	}{
		{"no models", func(c *config.Config) { c.Models = nil }},
		{"model without backends", func(c *config.Config) {
			m := c.Models["qwen-coder"]
			m.Backends = nil
			c.Models["qwen-coder"] = m
		}},
		{"unknown backend ref", func(c *config.Config) {
			m := c.Models["qwen-coder"]
			m.Backends = []config.BackendRef{{Name: "ghost"}}
			c.Models["qwen-coder"] = m
		}},
		{"unsupported strategy", func(c *config.Config) {
			m := c.Models["qwen-coder"]
			m.Strategy = "latency-learning"
			c.Models["qwen-coder"] = m
		}},
	}
	for _, tc := range cases {
		cfg := testConfig()
		tc.mut(cfg)
		if _, err := New(cfg, nil); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
}
