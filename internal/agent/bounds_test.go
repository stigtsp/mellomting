package agent_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/agent"
	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// listServer serves a fixed /v1/models body, counts how often it is
// asked, and answers completions with body.
func listServer(t *testing.T, models, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			hits.Add(1)
			fmt.Fprint(w, models)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func cards(ids ...string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf(`{"object":"model","id":%q}`, id)
	}
	return `{"object":"list","data":[` + strings.Join(parts, ",") + `]}`
}

// waitDiscovered waits until the edge has asked the agent's server for
// its list through an attachment (the agent's own probe is the first).
func waitDiscovered(t *testing.T, hits *atomic.Int32) {
	t.Helper()
	waitFor(t, "an attach attempt", func() bool { return hits.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
}

func TestAgentTooManyModelsRefused(t *testing.T) {
	e := newEdge(t, config.Agents{MaxModels: 2}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	be, hits := listServer(t, cards("a", "b", "c"), "")
	e.runAgent(t, "gpu", be.URL)
	waitDiscovered(t, hits)
	if got := e.router.List(); len(got) != 0 {
		t.Fatalf("a list over agents.max_models must not register anything, got %v", got)
	}
}

func TestAgentMalformedModelListRefused(t *testing.T) {
	for name, list := range map[string]string{
		"not json":        `{"object":"list","data":[`,
		"duplicate ids":   cards("a", "a"),
		"bad window":      `{"object":"list","data":[{"object":"model","id":"a","max_model_len":"big"}]}`,
		"wrong object":    `{"object":"thing","data":[{"object":"model","id":"a"}]}`,
		"non-ascii id":    cards("café"),
		"wildcard id":     cards("*"),
		"namespace grant": cards("x/*"),
		"comma in an id":  cards("a,b"),
		"oversized id":    cards(strings.Repeat("x", 300)),
		"second document": cards("a") + cards("b"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
			be, hits := listServer(t, list, "")
			e.runAgent(t, "gpu", be.URL)
			// A list the agent itself refuses never reaches the edge;
			// either way nothing may be registered.
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if got := e.router.List(); len(got) != 0 {
					t.Fatalf("registered %v from a malformed list", got)
				}
				time.Sleep(10 * time.Millisecond)
			}
			_ = hits
		})
	}
}

// A configured model always wins over an agent advertising the same name,
// through the real attach path.
func TestAgentCannotShadowConfiguredModel(t *testing.T) {
	static, _ := listServer(t, cards("m1"), `{"id":"s","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from static"},"finish_reason":"stop"}]}`)
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	}, edgeOptions{extraYAML: fmt.Sprintf(`servers:
  static: {url: %q}
models:
  lab/m1: {servers: [static], upstream_model: m1}
`, static.URL)})
	be, hits := listServer(t, cards("m1", "m2"), `{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from agent"},"finish_reason":"stop"}]}`)
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "lab/m2 from the agent", func() bool { return e.router.Has("lab/m2") })
	_ = hits
	for range 5 {
		code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false))
		if code != 200 || !strings.Contains(body, "from static") {
			t.Fatalf("lab/m1 must stay on the configured server: %d %s", code, body)
		}
	}
}

// An agent's buffered response is bounded by server.max_response_bytes
// like any backend's.
func TestAgentResponseSizeBounded(t *testing.T) {
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	}, edgeOptions{maxResponseBytes: 4096})
	huge := `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("x", 64<<10) + `"},"finish_reason":"stop"}]}`
	be, _ := listServer(t, cards("m1"), huge)
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false))
	if code == 200 || len(body) > 4096 {
		t.Fatalf("oversized agent response relayed: %d, %d bytes", code, len(body))
	}
}

// Revocation through the real path: a reloaded key store without the
// agent key, then the check the daemon runs after every reload.
func TestAgentRevokedThroughReload(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	be, _ := listServer(t, cards("m1"), "")
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	kept := &auth.UsersFile{Version: 1}
	for _, k := range e.users.Keys {
		if k.Name != "gpu" {
			kept.Keys = append(kept.Keys, k)
		}
	}
	store, err := auth.NewStore(kept, e.pepper)
	if err != nil {
		t.Fatal(err)
	}
	e.api.ReloadStore(store)
	e.registry.CheckKeys()
	waitFor(t, "model to leave", func() bool { return !e.router.Has("lab/m1") })
	if code := attachStatus(t, e, e.keys["gpu"]); code != http.StatusUnauthorized {
		t.Fatalf("revoked key attach: got %d, want 401", code)
	}
}

// With agents disabled the attach path does not exist for an
// authenticated caller, and anonymous callers are refused first.
func TestAttachWithAgentsDisabled(t *testing.T) {
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}}, edgeOptions{noAgents: true})
	if code := attachStatus(t, e, e.keys["gpu"]); code != http.StatusNotFound {
		t.Fatalf("attach with agents disabled: got %d, want 404", code)
	}
	if code := attachStatus(t, e, ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous attach: got %d, want 401", code)
	}
	_ = agent.AttachPath
}
