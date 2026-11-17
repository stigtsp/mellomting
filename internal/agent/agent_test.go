package agent_test

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/agent"
	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
	"mellomting/internal/testsupport"
)

// edge is an in-process Mellomting edge with agents enabled and no
// configured models, on a real TCP listener (attach needs a hijackable
// connection).
type edge struct {
	url      *url.URL
	router   *routing.Router
	registry *agent.Registry
	api      *httpapi.Server
	active   sync.Map // key ID -> false when deactivated
	keys     map[string]string
	users    *auth.UsersFile
	pepper   []byte
}

// edgeOptions vary the test edge: extra YAML (static servers and models,
// server limits) and whether the agent path is wired at all.
type edgeOptions struct {
	extraYAML        string
	noAgents         bool
	maxResponseBytes int
	maxInflight      int
	log              *slog.Logger
}

// fakeBackend is an OpenAI-shaped inference server advertising ids. Chat
// completions echo the model they were asked for, so a test can check
// the upstream name the edge rewrote.
func fakeBackend(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			data := make([]map[string]string, 0, len(ids))
			for _, id := range ids {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "credential leaked to backend", http.StatusBadRequest)
				return
			}
			var req struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				for i := range 3 {
					fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d\"}}]}\n\n", req.Model, i)
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hello from %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, req.Model, req.Model)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newEdge builds the edge. keys maps key names to their users-file
// record (ID, secret hash and name are filled in); the raw keys are
// returned in edge.keys.
func newEdge(t *testing.T, agents config.Agents, keys map[string]auth.Key) *edge {
	t.Helper()
	return newEdgeWith(t, agents, keys, edgeOptions{})
}

func newEdgeWith(t *testing.T, agents config.Agents, keys map[string]auth.Key, opts edgeOptions) *edge {
	t.Helper()
	dir := t.TempDir()
	if agents.MaxAttachments == 0 {
		agents.MaxAttachments = 16
	}
	if agents.MaxAttachmentsPerKey == 0 {
		agents.MaxAttachmentsPerKey = agents.MaxAttachments
	}
	cfg, err := config.Parse(fmt.Appendf(nil, `version: 1
server:
  listen: {network: tcp, address: "127.0.0.1:0"}
  max_response_bytes: %d
  max_inflight_requests: %d
auth:
  users_file: %s/users.yaml
  pepper_file: %s/auth.pepper
agents:
  enabled: true
  max_attachments: %d
  max_attachments_per_key: %d
  max_models: %d
  max_concurrency: %d
%s`, cmp.Or(opts.maxResponseBytes, 16<<20), cmp.Or(opts.maxInflight, 64), dir, dir,
		agents.MaxAttachments, agents.MaxAttachmentsPerKey, cmp.Or(agents.MaxModels, 16), cmp.Or(agents.MaxConcurrency, 8), opts.extraYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	pepper := []byte("0123456789abcdef0123456789abcdef")
	uf := &auth.UsersFile{Version: 1}
	raw := make(map[string]string, len(keys))
	for name, k := range keys {
		key, id, err := auth.Generate(name)
		if err != nil {
			t.Fatal(err)
		}
		k.ID, k.Name, k.Enabled = id, name, true
		k.SecretHash = auth.FormatHashValue(auth.Hash(pepper, key))
		uf.Keys = append(uf.Keys, k)
		raw[name] = key
	}
	store, err := auth.NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}

	e := &edge{keys: raw, users: uf, pepper: pepper}
	log := cmp.Or(opts.log, testsupport.DiscardLogger())
	clients := map[string]*backend.Client{}
	for name, b := range cfg.Backends {
		c, err := backend.New(backend.Options{Name: name, Cfg: b, Network: backend.Policy{Mode: "loopback-only"},
			MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		clients[name] = c
	}
	e.router, err = routing.New(cfg, func(name string) int {
		if c, ok := clients[name]; ok {
			return c.Inflight()
		}
		return e.registry.Inflight(name)
	})
	if err != nil {
		t.Fatal(err)
	}
	prox, err := proxy.New(cfg, e.router, clients, log, accounting.NewQuota(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(cfg, log, store, e.router, prox)
	e.api = api
	e.registry = agent.NewRegistry(cfg.Agents, e.router, cfg.Server.MaxResponseBytes, log)
	e.registry.SetActive(func(k *auth.Key) bool {
		if v, ok := e.active.Load(k.ID); ok && !v.(bool) {
			return false
		}
		return api.KeyActive(k)
	})
	if !opts.noAgents {
		prox.SetAgents(e.registry)
		api.SetAgents(e.registry)
	}

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(func() { e.registry.Close(); ts.Close() })
	e.url, _ = url.Parse(ts.URL)
	return e
}

// runAgent attaches backend to the edge with the named key until the
// test ends or the returned cancel is called.
func (e *edge) runAgent(t *testing.T, keyName, backendURL string) context.CancelFunc {
	t.Helper()
	return e.runAgentVia(t, keyName, backendURL, e.url)
}

// runAgentVia is runAgent with the agent dialing edgeURL, e.g. a relay
// in front of the edge. Cleanup waits for the agent to stop completely.
func (e *edge) runAgentVia(t *testing.T, keyName, backendURL string, edgeURL *url.URL) context.CancelFunc {
	t.Helper()
	b, _ := url.Parse(backendURL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Run(ctx, agent.Options{Edge: edgeURL, Key: e.keys[keyName], Backend: b, Log: testsupport.DiscardLogger()})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func (e *edge) do(t *testing.T, method, path, keyName, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.url.String()+path, strings.NewReader(body))
	if keyName != "" {
		req.Header.Set("Authorization", "Bearer "+e.keys[keyName])
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func chat(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, model, stream)
}

func TestAgentAttachServesNamespacedModels(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
		"carol": {Models: []string{"other/*"}},
	})
	be := fakeBackend(t, "m1", "org/m2")
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "models to register", func() bool { return e.router.Has("lab/m1") && e.router.Has("lab/org/m2") })

	// The namespace comes from the key: the bare names are not routable.
	if e.router.Has("m1") {
		t.Fatal("advertised name registered without the key's namespace")
	}

	code, body := e.do(t, "GET", "/v1/models", "alice", "")
	if code != 200 || !strings.Contains(body, `"lab/m1"`) || !strings.Contains(body, `"lab/org/m2"`) {
		t.Fatalf("models for alice: %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/v1/models", "carol", "")
	if code != 200 || strings.Contains(body, "lab/") {
		t.Fatalf("models for carol must not list lab/: %d %s", code, body)
	}

	// Non-stream: the edge rewrites the public name to the upstream id.
	code, body = e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/org/m2", false))
	if code != 200 || !strings.Contains(body, "hello from org/m2") {
		t.Fatalf("chat: %d %s", code, body)
	}
	// Stream: every event and the terminator arrive.
	code, body = e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", true))
	if code != 200 || strings.Count(body, "data: ") != 4 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream: %d %q", code, body)
	}
	// ACL: carol may not use lab/ models; unknown and denied read alike.
	if code, _ := e.do(t, "POST", "/v1/chat/completions", "carol", chat("lab/m1", false)); code != 404 {
		t.Fatalf("carol chat: got %d, want 404", code)
	}
}

func TestAgentKeyIsolation(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"*"}},
	})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	// An agent key does nothing but attach.
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		method, body := "GET", ""
		if path != "/v1/models" {
			method, body = "POST", chat("lab/m1", false)
		}
		if code, _ := e.do(t, method, path, "gpu", body); code != 401 {
			t.Errorf("agent key on %s: got %d, want 401", path, code)
		}
	}
	// A client key, even an all-models one, cannot attach.
	if code := attachStatus(t, e, e.keys["alice"]); code != 401 {
		t.Errorf("client key attach: got %d, want 401", code)
	}
	if code := attachStatus(t, e, ""); code != 401 {
		t.Errorf("anonymous attach: got %d, want 401", code)
	}
	// Without the upgrade headers the agent key gets 426.
	if code, _ := e.do(t, "GET", agent.AttachPath, "gpu", ""); code != http.StatusUpgradeRequired {
		t.Errorf("attach without upgrade: got %d, want 426", code)
	}
}

// attachStatus performs the attach handshake with key and returns the
// response status.
func attachStatus(t *testing.T, e *edge, key string) int {
	t.Helper()
	conn, err := net.Dial("tcp", e.url.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req, _ := http.NewRequest("GET", e.url.String()+agent.AttachPath, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", agent.UpgradeProtocol)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestAgentDetachRemovesModels(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	stop := e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	stop()
	waitFor(t, "model to leave", func() bool { return !e.router.Has("lab/m1") })
	if code, _ := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 404 {
		t.Fatalf("chat after detach: got %d, want 404", code)
	}
}

func TestAgentKeyDeactivationDetaches(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	parsed, err := auth.Parse(e.keys["gpu"])
	if err != nil {
		t.Fatal(err)
	}
	e.active.Store(parsed.ID, false)
	e.registry.CheckKeys()
	waitFor(t, "model to leave", func() bool { return !e.router.Has("lab/m1") })
}

func TestAgentReplicasShareAModel(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "both agents to attach", func() bool {
		_, ok1 := e.registry.Client("agent-gpu-1")
		_, ok2 := e.registry.Client("agent-gpu-2")
		return ok1 && ok2 && e.router.Has("lab/m1")
	})
	if got := e.router.List(); len(got) != 1 || got[0] != "lab/m1" {
		t.Fatalf("one public model expected, got %v", got)
	}
	for range 4 {
		if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 200 {
			t.Fatalf("chat: %d %s", code, body)
		}
	}
}

func TestAgentWildcardNamespace(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: config.AgentNamespaceWildcard},
		"alice": {Models: []string{"m1"}},
	})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("m1") })
	if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("m1", false)); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
}

func TestAgentAttachmentLimit(t *testing.T) {
	e := newEdge(t, config.Agents{MaxAttachments: 1}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	e.runAgent(t, "gpu", fakeBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	if code := attachStatus(t, e, e.keys["gpu"]); code != http.StatusServiceUnavailable {
		t.Fatalf("attach beyond max_attachments: got %d, want 503", code)
	}
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
