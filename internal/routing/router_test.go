package routing

import (
	"testing"

	"mellomting/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Backends: map[string]config.Backend{
			"back-a": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "Qwen/Qwen3-Coder"},
			"back-b": {BaseURL: "http://127.0.0.1:8002", UpstreamModel: "Qwen/Qwen3-Coder"},
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

func TestResolveFirstBackend(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.Resolve("qwen-coder")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Backend != "back-a" {
		t.Fatalf("backend = %q, want back-a (deterministic first replica)", t1.Backend)
	}
	if t1.Upstream != "Qwen/Qwen3-Coder" || t1.PublicModel != "qwen-coder" {
		t.Fatalf("target = %+v", t1)
	}
	t2, err := r.Resolve("embed-small")
	if err != nil {
		t.Fatal(err)
	}
	if t2.Backend != "back-b" {
		t.Fatalf("embedding backend = %q, want back-b", t2.Backend)
	}
}

func TestResolveUnknown(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("nope"); err != ErrUnknownModel {
		t.Fatalf("err = %v", err)
	}
	// provider/model syntax must not implicitly resolve.
	if _, err := r.Resolve("provider/qwen-coder"); err != ErrUnknownModel {
		t.Fatalf("provider/model syntax accepted: %v", err)
	}
	if r.Has("qwen-coder") != true || r.Has("nope") != false {
		t.Fatal("Has wrong")
	}
}

func TestListSorted(t *testing.T) {
	t.Parallel()
	r, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := r.List()
	want := []string{"embed-small", "qwen-coder"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %v", got)
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
		if _, err := New(cfg); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
}
