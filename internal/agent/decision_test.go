package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/agent"
	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/testsupport"
)

// fakeDecision is a SystemOne decision server like the Karpo and Clef
// servers: a health path and POST /v1/systemone, which rejects any key
// but model, state and questions; nothing else, not even /v1/models.
type fakeDecision struct {
	*httptest.Server
	healthy atomic.Bool
	mu      sync.Mutex
	bodies  []map[string]json.RawMessage
	other   atomic.Int32 // requests to any other route
}

func newFakeDecision(t *testing.T, model string) *fakeDecision {
	t.Helper()
	f := &fakeDecision{}
	f.healthy.Store(true)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			if !f.healthy.Load() {
				http.Error(w, "loading", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "ok")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/systemone":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid JSON"}`)
				return
			}
			f.mu.Lock()
			f.bodies = append(f.bodies, body)
			f.mu.Unlock()
			for k := range body {
				if k != "model" && k != "state" && k != "questions" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"error":"unknown key %s"}`, k)
					return
				}
			}
			if string(body["model"]) != fmt.Sprintf("%q", model) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"unknown model"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":%q,"answers":{"q1":{"probability":0.93}},"usage":{"input_tokens":11,"output_tokens":0}}`, model)
		default:
			f.other.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDecision) received() []map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bodies)
}

// runDecisionAgent attaches a decision server, as attach.decision_servers
// configures one.
func (e *edge) runDecisionAgent(t *testing.T, keyName, backendURL string, models ...string) {
	t.Helper()
	b, _ := url.Parse(backendURL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Run(ctx, agent.Options{Edge: e.url, Key: e.keys[keyName], Backend: b,
			Decision: &agent.Decision{Models: models, Health: "/healthz"}, Log: testsupport.DiscardLogger()})
	}()
	t.Cleanup(func() { cancel(); <-done })
}

const decideBody = `{"model":"lab/karpo","state":"open(FH, $ARGV[0]);","questions":{"q1":{"type":"noul","instructions":"malicious?"}}}`

// SYSTEMONE_PLAN §D13 end to end: an agent declares a decision server,
// the edge registers its model typed decision, and /v1/systemone reaches
// the server with the body unchanged but for the model.
func TestDecisionAgentServesSystemOne(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	f := newFakeDecision(t, "karpo")
	e.runDecisionAgent(t, "gpu", f.URL, "karpo")
	waitFor(t, "decision model to register", func() bool { return e.router.Has("lab/karpo") })

	if got := e.router.TypeOf("lab/karpo"); got != "decision" {
		t.Fatalf("type = %q, want decision", got)
	}
	snap := e.registry.Snapshot()
	if len(snap) != 1 || len(snap[0].Models) != 1 || snap[0].Models[0].Type != "decision" {
		t.Fatalf("snapshot = %+v, want one decision model", snap)
	}

	code, body := e.do(t, "POST", "/v1/systemone", "alice", decideBody)
	want := `{"model":"karpo","answers":{"q1":{"probability":0.93}},"usage":{"input_tokens":11,"output_tokens":0}}`
	if code != 200 || body != want {
		t.Fatalf("systemone: %d %s, want 200 %s", code, body, want)
	}
	got := f.received()
	if len(got) != 1 {
		t.Fatalf("server received %d calls, want 1", len(got))
	}
	keys := make([]string, 0, len(got[0]))
	for k := range got[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"model", "questions", "state"}) || string(got[0]["model"]) != `"karpo"` {
		t.Fatalf("forwarded body = %v, want the client's keys with model rewritten", got[0])
	}

	// stream is refused at the edge, never forwarded.
	streamed := strings.Replace(decideBody, `{"model"`, `{"stream":true,"model"`, 1)
	if code, body := e.do(t, "POST", "/v1/systemone", "alice", streamed); code != 400 {
		t.Fatalf("stream: %d %s, want 400", code, body)
	}
	// A decision model is refused on the generation and embeddings routes.
	if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/karpo", false)); code != 400 || !strings.Contains(body, "model_type_mismatch") {
		t.Fatalf("chat on a decision model: %d %s, want 400 model_type_mismatch", code, body)
	}
	if code, _ := e.do(t, "POST", "/v1/embeddings", "alice", `{"model":"lab/karpo","input":"x"}`); code != 400 {
		t.Fatalf("embeddings on a decision model: %d, want 400", code)
	}
	// The server's rejection reaches the client (relay on by default).
	bad := strings.Replace(decideBody, `"state"`, `"extra":1,"state"`, 1)
	if code, body := e.do(t, "POST", "/v1/systemone", "alice", bad); code != 400 || !strings.Contains(body, "unknown key extra") {
		t.Fatalf("rejection: %d %s, want 400 relaying the server's reason", code, body)
	}
	if n := len(f.received()); n != 2 {
		t.Fatalf("server received %d calls, want 2 (the stream and type refusals stay at the edge)", n)
	}
	// The agent asked the server for nothing but its health and
	// /v1/systemone: no /v1/models, no load report.
	if n := f.other.Load(); n != 0 {
		t.Fatalf("server got %d requests on other routes", n)
	}
}

// The agent attaches once the health path answers 2xx, as a generation
// agent waits for its model list.
func TestDecisionAgentWaitsForHealth(t *testing.T) {
	// Cleanups run last-in first-out: the timings are restored after the
	// agent has stopped, not while it still reads them.
	t.Cleanup(agent.SetTimingsForTest(20 * time.Millisecond))
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	f := newFakeDecision(t, "karpo")
	f.healthy.Store(false)
	e.runDecisionAgent(t, "gpu", f.URL, "karpo")
	if e.router.Has("lab/karpo") {
		t.Fatal("registered before the server was healthy")
	}
	f.healthy.Store(true)
	waitFor(t, "decision model to register", func() bool { return e.router.Has("lab/karpo") })
}

// Types come only from the agent's own answer on ModelTypesPath. A
// generation server that answers that path itself is never asked, since
// the agent does not relay it, so its models stay untyped and stay off
// /v1/systemone.
func TestServerCannotTypeItsOwnModels(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	var asked atomic.Int32
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m1","object":"model"}]}`)
		case agent.ModelTypesPath:
			asked.Add(1)
			_, _ = io.WriteString(w, `{"decision":["m1"]}`)
		case "/v1/systemone":
			asked.Add(1)
			_, _ = io.WriteString(w, `{"answers":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(be.Close)
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	if got := e.router.TypeOf("lab/m1"); got != "" {
		t.Fatalf("type = %q, want untyped", got)
	}
	if code, _ := e.do(t, "POST", "/v1/systemone", "alice", `{"model":"lab/m1","state":"x","questions":{}}`); code != 400 {
		t.Fatalf("systemone on a generation model: %d, want 400", code)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("server was asked %d times on decision paths", n)
	}
}
