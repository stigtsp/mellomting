package agent_test

import (
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
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/agent"
	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// switchableBackend is an inference server that can go down (503) and
// change the models it advertises.
type switchableBackend struct {
	*httptest.Server
	down   atomic.Bool
	mu     sync.Mutex
	models []string
}

func newSwitchableBackend(t *testing.T, models ...string) *switchableBackend {
	t.Helper()
	b := &switchableBackend{models: models}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		b.mu.Lock()
		ids := append([]string(nil), b.models...)
		b.mu.Unlock()
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *switchableBackend) setModels(ids ...string) {
	b.mu.Lock()
	b.models = ids
	b.mu.Unlock()
}

func shortTimings(t *testing.T) {
	t.Cleanup(agent.SetTimingsForTest(200 * time.Millisecond))
}

func TestAgentDetachesWhileBackendIsDownAndReturns(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	be := newSwitchableBackend(t, "m1")
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	be.down.Store(true)
	waitFor(t, "model to leave while the backend is down", func() bool { return !e.router.Has("lab/m1") })
	be.down.Store(false)
	waitFor(t, "model to return with the backend", func() bool { return e.router.Has("lab/m1") })
}

func TestAgentReattachesWhenModelsChange(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	be := newSwitchableBackend(t, "m1")
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "m1 to register", func() bool { return e.router.Has("lab/m1") })

	be.setModels("m2")
	waitFor(t, "m2 to replace m1", func() bool { return e.router.Has("lab/m2") && !e.router.Has("lab/m1") })
}

func TestAgentReconnectsAfterEdgeDropsIt(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	e.runAgent(t, "gpu", newSwitchableBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	_, ok := e.registry.Client("agent-gpu-1")
	if !ok {
		t.Fatal("first attachment missing")
	}

	// The edge drops every attachment of the key once; the key itself is
	// still valid, so the agent must come back on its own.
	parsed, _ := auth.Parse(e.keys["gpu"])
	e.active.Store(parsed.ID, false)
	e.registry.CheckKeys()
	e.active.Store(parsed.ID, true)
	waitFor(t, "a new attachment", func() bool {
		_, again := e.registry.Client("agent-gpu-2")
		return again && e.router.Has("lab/m1")
	})
}

// relay forwards TCP between a client and target until it is paused,
// after which it silently swallows everything: a network black hole, not
// a reset.
type relay struct {
	ln        net.Listener
	paused    atomic.Bool
	refuseNew atomic.Bool // new connections are closed at once; existing ones keep working
}

func newRelay(t *testing.T, target string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c, target)
		}
	}()
	return r
}

func (r *relay) serve(c net.Conn, target string) {
	if r.refuseNew.Load() {
		_ = c.Close()
		return
	}
	if r.paused.Load() {
		// New connections hang too, like a real outage.
		_, _ = io.Copy(io.Discard, c)
		_ = c.Close()
		return
	}
	u, err := net.Dial("tcp", target)
	if err != nil {
		_ = c.Close()
		return
	}
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if err != nil {
				_ = dst.Close()
				_ = src.Close()
				return
			}
			if r.paused.Load() {
				continue
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				_ = src.Close()
				return
			}
		}
	}
	go pipe(u, c)
	go pipe(c, u)
}

func TestAgentSurvivesSilentNetworkLoss(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	rl := newRelay(t, e.url.Host)
	via, _ := url.Parse("http://" + rl.ln.Addr().String())

	e.runAgentVia(t, "gpu", newSwitchableBackend(t, "m1").URL, via)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	// Black-hole the path: no FIN, no RST. Keepalive pings must notice on
	// the edge, which drops the model.
	rl.paused.Store(true)
	waitFor(t, "edge to drop the silent agent", func() bool { return !e.router.Has("lab/m1") })

	// When the network returns, the agent must notice its own dead
	// connection and attach again.
	rl.paused.Store(false)
	waitFor(t, "agent to reattach after the outage", func() bool { return e.router.Has("lab/m1") })
}

// A server that keeps answering requests stays attached even when its
// health probe fails (busy servers answer /v1/models slowly), and is
// detached once it stops serving too.
func TestAgentKeepsBusyBackendAttached(t *testing.T) {
	shortTimings(t)
	var probesFail atomic.Bool
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if probesFail.Load() {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"object":"list","data":[{"object":"model","id":"m1"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(be.Close)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	probesFail.Store(true)
	deadline := time.Now().Add(1500 * time.Millisecond) // several watch intervals
	for time.Now().Before(deadline) {
		if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 200 {
			t.Fatalf("a busy but serving backend was cut off: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// No traffic and failing probes: now it must go.
	waitFor(t, "an idle failing backend to detach", func() bool { return !e.router.Has("lab/m1") })
}

// The edge's log tells a graceful detach (the agent sent GOAWAY because
// its server went down) from a lost connection.
func TestEdgeLogsWhyAnAgentDetached(t *testing.T) {
	shortTimings(t)
	var logs syncBuffer
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}},
		edgeOptions{log: slog.New(slog.NewTextHandler(&logs, nil))})
	be := newSwitchableBackend(t, "m1")
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	be.down.Store(true)
	defer func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	waitFor(t, "graceful detach to be logged", func() bool {
		return strings.Contains(logs.String(), `msg="agent detached"`) &&
			strings.Contains(logs.String(), `reason="agent detached gracefully"`)
	})
	if strings.Contains(logs.String(), "connection closed or lost") {
		t.Fatalf("graceful detach logged as a lost connection:\n%s", logs.String())
	}
}

// A server that comes up with an unusable model list is reported as
// such, not hidden behind its earlier failure, and the log names the
// entry by position, never echoing the untrusted ID.
func TestAgentReportsUnusableModelList(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}})
	be := newSwitchableBackend(t, "ok", "bad\u202eid")
	be.down.Store(true)
	var logs syncBuffer
	b, _ := url.Parse(be.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Run(ctx, agent.Options{Edge: e.url, Key: e.keys["gpu"], Backend: b,
			Log: slog.New(slog.NewTextHandler(&logs, nil))})
	}()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the 503 to be logged", func() bool { return strings.Contains(logs.String(), "503") })
	be.down.Store(false)
	defer func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	waitFor(t, "the unusable list to be logged", func() bool {
		return strings.Contains(logs.String(), "model list is unusable") &&
			strings.Contains(logs.String(), "(entry 2)")
	})
	if strings.Contains(logs.String(), "bad") {
		t.Fatal("the backend's model ID was echoed in the log")
	}
	if e.router.Has("lab/ok") {
		t.Fatal("a rejected list registered models")
	}
}
