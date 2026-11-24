package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
	"mellomting/internal/testsupport"
)

// fakeAgents is the attached-agent table: a name absent from it is an
// attachment that has detached.
type fakeAgents map[string]*backend.Client

func (f fakeAgents) Client(name string) (*backend.Client, bool) { c, ok := f[name]; return c, ok }
func (f fakeAgents) DrainAll()                                  {}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// agentClient is an attached agent's backend client answering every
// request with a chat completion, over an in-memory transport.
func agentClient(t *testing.T, cfg *config.Config, name string) *backend.Client {
	t.Helper()
	c, err := backend.New(backend.Options{
		Name:             name,
		Cfg:              config.AgentBackend(cfg.Agents),
		Network:          backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes,
		Log:              testsupport.DiscardLogger(),
		Untrusted:        true,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"id":"chatcmpl-1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// agentProxy builds a proxy with agents enabled. routes are the attached
// backends serving "lab/m"; agents holds the ones still attached. The
// router's load function makes the detached ones look idlest, so
// least-inflight picks them first.
func agentProxy(t *testing.T, routes []string, agents func(*config.Config) fakeAgents) (*Proxy, *routing.Router) {
	t.Helper()
	cfg := testConfig("http://127.0.0.1:1")
	cfg.Agents = config.Agents{Enabled: true, MaxAttachments: 4, MaxAttachmentsPerKey: 4, MaxConcurrency: 4,
		MaxModels: 4, QueueSize: 4, QueueTimeout: config.Duration(time.Second)}
	table := agents(cfg)
	router, err := routing.New(cfg, func(name string) int {
		if _, ok := table[name]; ok {
			return 1
		}
		return 0
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range routes {
		if err := router.AddDynamic("lab/m", name, "m", 0, "AK"); err != nil {
			t.Fatal(err)
		}
	}
	static, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Transport: transportFor(cfg.Backends["b1"].BaseURL), Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, router, map[string]*backend.Client{"b1": static}, testsupport.DiscardLogger(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.SetAgents(table)
	return p, router
}

func labKey() *auth.Key {
	return &auth.Key{ID: "K1", Name: "t", Enabled: true, Models: []string{"lab/*"}}
}

// A replica that detached between selection and forwarding is skipped
// for another, without spending the request's only attempt.
func TestDetachedReplicaFallsBack(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1", "agent-a-2"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-2": agentClient(t, cfg, "agent-a-2")}
	})
	w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
}

// With every replica detached the request is a 503 no-backend, not an
// internal error.
func TestDetachedOnlyReplicaIsUnavailable(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(*config.Config) fakeAgents { return fakeAgents{} })
	w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no backend is available") {
		t.Fatalf("status %d, want 503 no backend: %s", w.Code, w.Body)
	}
}

// A replica whose attachment drained (detach releases its queue) is
// skipped like a detached one.
func TestDrainedReplicaFallsBack(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1", "agent-a-2"}, func(cfg *config.Config) fakeAgents {
		drained := agentClient(t, cfg, "agent-a-1")
		drained.Drain()
		return fakeAgents{"agent-a-1": drained, "agent-a-2": agentClient(t, cfg, "agent-a-2")}
	})
	// Both look equally loaded here; least-inflight takes the first.
	w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
}

// A stored response pinned to an attachment that has detached is not
// found, on both the continuation and the retrieval routes.
func TestResponsePinnedToDetachedAgent(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-2"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-2": agentClient(t, cfg, "agent-a-2")}
	})
	p.affinity.Put("K1", "resp_gone", "agent-a-1", "lab/m")

	w := run(t, p, http.MethodGet, "/v1/responses/resp_gone", "", labKey())
	if w.Code != http.StatusNotFound {
		t.Fatalf("retrieve: status %d, want 404: %s", w.Code, w.Body)
	}
	w = run(t, p, http.MethodPost, "/v1/responses", `{"model":"lab/m","input":"x","previous_response_id":"resp_gone"}`, labKey())
	if w.Code != http.StatusNotFound {
		t.Fatalf("continue: status %d, want 404: %s", w.Code, w.Body)
	}
}

// An attached model has no type: its server's model list does not say
// whether it embeds, so the embeddings endpoint reaches it too.
func TestAttachedModelServesEmbeddings(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-1": agentClient(t, cfg, "agent-a-1")}
	})
	w := run(t, p, http.MethodPost, "/v1/embeddings", `{"model":"lab/m","input":"x"}`, labKey())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
}

// A replica whose connection refused the request before sending it (a
// GOAWAY the edge has not yet acted on) is skipped like a detached one;
// the request is not failed with a connect error.
func TestGoneConnectionFallsBack(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1", "agent-a-2"}, func(cfg *config.Config) fakeAgents {
		gone, err := backend.New(backend.Options{
			Name: "agent-a-1", Cfg: config.AgentBackend(cfg.Agents), Network: backend.Policy{Mode: "loopback-only"},
			MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(), Untrusted: true,
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, backend.ErrGone }),
		})
		if err != nil {
			t.Fatal(err)
		}
		return fakeAgents{"agent-a-1": gone, "agent-a-2": agentClient(t, cfg, "agent-a-2")}
	})
	w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
}

// An attached model's type is unknown, and a decision server advertises
// no model list to attach from, so /v1/systemone never reaches one: the
// decision endpoint serves only models configured as type: decision.
func TestAttachedModelRefusedOnDecision(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-1": agentClient(t, cfg, "agent-a-1")}
	})
	w := run(t, p, http.MethodPost, "/v1/systemone", `{"model":"lab/m","input":"x"}`, labKey())
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "model_type_mismatch") {
		t.Fatalf("status %d, want 400 model_type_mismatch: %s", w.Code, w.Body)
	}
}
