package agent_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// An agent key's register list is an allowlist with aliases (PLAN
// §104.4): only listed ids register, under their alias, and requests
// reach the server under the advertised id.
func TestAgentRegisterAllowlistAndAlias(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"ollama": {Agent: config.AgentNamespaceWildcard, Register: []auth.AgentModel{
			{ID: "qwen3-coder:30b", As: "coder"},
			{ID: "org/m2", As: "m2"},
			{ID: "plain:7b"},
		}},
		"alice": {Models: []string{"*"}},
	})
	be := fakeBackend(t, "qwen3-coder:30b", "org/m2", "plain:7b", "other:1b", "llama3:8b")
	e.runAgent(t, "ollama", be.URL)
	waitFor(t, "listed models to register", func() bool {
		return e.router.Has("coder") && e.router.Has("m2") && e.router.Has("plain:7b")
	})
	for _, name := range []string{"qwen3-coder:30b", "org/m2", "other:1b", "llama3:8b"} {
		if e.router.Has(name) {
			t.Errorf("%q registered, but only the listed ids under their aliases may be", name)
		}
	}
	if got := e.router.List(); len(got) != 3 {
		t.Fatalf("public models = %v, want exactly the three listed", got)
	}
	// The edge rewrites the alias to the advertised id upstream.
	code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("coder", false))
	if code != 200 || !strings.Contains(body, "hello from qwen3-coder:30b") {
		t.Fatalf("chat coder: %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/v1/chat/completions", "alice", chat("m2", true))
	if code != 200 || !strings.Contains(body, `"model":"org/m2"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream m2: %d %q", code, body)
	}
	if code, _ := e.do(t, "POST", "/v1/chat/completions", "alice", chat("other:1b", false)); code != 404 {
		t.Fatalf("unlisted model: got %d, want 404", code)
	}
}

// Under a namespace, aliases keep the prefix.
func TestAgentRegisterUnderNamespace(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab", Register: []auth.AgentModel{{ID: "big/model-v3", As: "big"}}},
		"alice": {Models: []string{"lab/*"}},
	})
	e.runAgent(t, "gpu", fakeBackend(t, "big/model-v3", "small").URL)
	waitFor(t, "alias to register", func() bool { return e.router.Has("lab/big") })
	if e.router.Has("lab/small") || e.router.Has("lab/big/model-v3") {
		t.Fatalf("unlisted or unaliased name registered: %v", e.router.List())
	}
	if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/big", false)); code != 200 || !strings.Contains(body, "hello from big/model-v3") {
		t.Fatalf("chat: %d %s", code, body)
	}
}

// A server listing more models than agents.max_models still attaches
// when the register list picks few of them; without a list it cannot.
func TestAgentRegisterBeyondMaxModels(t *testing.T) {
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("model-%02d:latest", i)
	}
	var logs syncBuffer
	e := newEdgeWith(t, config.Agents{MaxModels: 2}, map[string]auth.Key{
		"listed":   {Agent: "a", Register: []auth.AgentModel{{ID: "model-07:latest", As: "seven"}}},
		"unlisted": {Agent: "b"},
	}, edgeOptions{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	be := fakeBackend(t, ids...)
	e.runAgent(t, "listed", be.URL)
	waitFor(t, "listed model to register", func() bool { return e.router.Has("a/seven") })

	e.runAgent(t, "unlisted", be.URL)
	waitFor(t, "unlisted agent to fail discovery", func() bool {
		return strings.Contains(logs.String(), "agent model discovery failed") && strings.Contains(logs.String(), "key=unlisted")
	})
	if got := e.router.List(); len(got) != 1 || got[0] != "a/seven" {
		t.Fatalf("public models = %v, want only a/seven", got)
	}
}

// Matches beyond max_models fail discovery, like an over-long list.
func TestAgentRegisterMatchesExceedMaxModels(t *testing.T) {
	var logs syncBuffer
	e := newEdgeWith(t, config.Agents{MaxModels: 1}, map[string]auth.Key{
		"gpu": {Agent: "lab", Register: []auth.AgentModel{{ID: "m1"}, {ID: "m2"}}},
	}, edgeOptions{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	e.runAgent(t, "gpu", fakeBackend(t, "m1", "m2", "m3").URL)
	waitFor(t, "discovery to fail", func() bool {
		return strings.Contains(logs.String(), "exceed agents.max_models")
	})
	if got := e.router.List(); len(got) != 0 {
		t.Fatalf("models registered although the matches exceed max_models: %v", got)
	}
}

// A listed id the server does not advertise is reported and skipped.
func TestAgentRegisterMissingID(t *testing.T) {
	var logs syncBuffer
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{
		"gpu": {Agent: "lab", Register: []auth.AgentModel{{ID: "m1"}, {ID: "gone:7b"}}},
	}, edgeOptions{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "m1 to register", func() bool { return e.router.Has("lab/m1") })
	waitFor(t, "missing id warning", func() bool { return strings.Contains(logs.String(), "gone:7b") })
}

// The motivating case: two Ollama hosts, each listing overlapping
// models, each serving one model at the root under its own key.
func TestAgentRegisterTwoHosts(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"ollama_a": {Agent: config.AgentNamespaceWildcard, Register: []auth.AgentModel{{ID: "qwen3-coder:30b", As: "coder"}}},
		"ollama_b": {Agent: config.AgentNamespaceWildcard, Register: []auth.AgentModel{{ID: "gpt-oss:20b"}}},
		"alice":    {Models: []string{"coder", "gpt-oss:20b"}},
	})
	e.runAgent(t, "ollama_a", fakeBackend(t, "qwen3-coder:30b", "gpt-oss:20b", "llama3:8b").URL)
	e.runAgent(t, "ollama_b", fakeBackend(t, "gpt-oss:20b", "qwen3-coder:30b", "llama3:8b").URL)
	waitFor(t, "both models to register", func() bool { return e.router.Has("coder") && e.router.Has("gpt-oss:20b") })
	if got := e.router.List(); len(got) != 2 {
		t.Fatalf("public models = %v, want coder and gpt-oss:20b only", got)
	}
	for _, m := range []string{"coder", "gpt-oss:20b"} {
		if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat(m, false)); code != 200 {
			t.Fatalf("chat %s: %d %s", m, code, body)
		}
	}
	code, body := e.do(t, "GET", "/v1/models", "alice", "")
	if code != 200 || strings.Contains(body, "llama3") || strings.Contains(body, "qwen3-coder:30b") {
		t.Fatalf("models listing: %d %s", code, body)
	}
}
