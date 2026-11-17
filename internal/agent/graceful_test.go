package agent_test

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// slowStreamer streams `chunks` events every `every` per completion, and
// lets a test change the advertised models or fail the model list.
type slowStreamer struct {
	*httptest.Server
	mu         sync.Mutex
	models     []string
	failModels atomic.Bool
}

func newSlowStreamer(t *testing.T, chunks int, every time.Duration, models ...string) *slowStreamer {
	t.Helper()
	s := &slowStreamer{models: models}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if s.failModels.Load() {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			s.mu.Lock()
			fmt.Fprint(w, cards(s.models...))
			s.mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range chunks {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *slowStreamer) setModels(ids ...string) {
	s.mu.Lock()
	s.models = ids
	s.mu.Unlock()
}

// startStream opens a streaming completion and returns a channel that
// reports whether it ended with [DONE].
func startStream(t *testing.T, e *edge, key, model string) <-chan bool {
	t.Helper()
	req, _ := http.NewRequest("POST", e.url.String()+"/v1/chat/completions", strings.NewReader(chat(model, true)))
	req.Header.Set("Authorization", "Bearer "+e.keys[key])
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	done := make(chan bool, 1)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		ok := false
		for sc.Scan() {
			ok = ok || sc.Text() == "data: [DONE]"
		}
		done <- ok
	}()
	return done
}

func streamResult(t *testing.T, done <-chan bool) bool {
	t.Helper()
	select {
	case ok := <-done:
		return ok
	case <-time.After(15 * time.Second):
		t.Fatal("stream never ended")
		return false
	}
}

// A model change reattaches make-before-break: the in-flight stream on
// the old attachment finishes, and the new model appears.
func TestAgentModelChangeDoesNotCutStreams(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	be := newSlowStreamer(t, 30, 50*time.Millisecond, "m1") // ~1.5 s
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "m1", func() bool { return e.router.Has("lab/m1") })

	done := startStream(t, e, "alice", "lab/m1")
	time.Sleep(200 * time.Millisecond)
	be.setModels("m1", "m2")
	waitFor(t, "m2 to appear", func() bool { return e.router.Has("lab/m2") })
	if !streamResult(t, done) {
		t.Fatal("the in-flight stream was cut by the model change")
	}
	if code, _ := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 200 {
		t.Fatalf("lab/m1 after the reattach: %d", code)
	}
}

// Stopping the agent drains: an in-flight stream finishes before the
// models go.
func TestAgentShutdownDrainsStreams(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	be := newSlowStreamer(t, 20, 50*time.Millisecond, "m1") // ~1 s
	stop := e.runAgent(t, "gpu", be.URL)
	waitFor(t, "m1", func() bool { return e.router.Has("lab/m1") })

	done := startStream(t, e, "alice", "lab/m1")
	time.Sleep(200 * time.Millisecond)
	go stop() // blocks until the agent has drained
	// The agent's GOAWAY takes it out of routing at once, while the
	// stream is still running...
	waitFor(t, "routing to stop", func() bool { return !e.router.Has("lab/m1") })
	select {
	case <-done:
		t.Fatal("routing stopped only when the stream ended, not on GOAWAY")
	default:
	}
	// ...so a new request gets a clean 404 rather than a failed forward,
	if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 404 {
		t.Fatalf("request during drain: %d %s", code, body)
	}
	// ...and the stream already running completes.
	if !streamResult(t, done) {
		t.Fatal("the in-flight stream was cut by the agent stopping")
	}
}

// A failing health probe never detaches an agent with a stream in
// flight, even when nothing has completed recently (a long prefill).
func TestAgentFailingProbeDuringLongStream(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	be := newSlowStreamer(t, 3, 600*time.Millisecond, "m1") // ~1.8 s, sparse events
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "m1", func() bool { return e.router.Has("lab/m1") })

	done := startStream(t, e, "alice", "lab/m1")
	be.failModels.Store(true)
	if !streamResult(t, done) {
		t.Fatal("the stream was cut while the health probe failed")
	}
}

// At the per-key attachment cap the new attachment cannot be added next
// to the old one; the agent replaces it in place. Streams in flight
// finish, the new model appears quickly, and the existing model is served
// again within a moment rather than after a long outage.
func TestAgentModelChangeAtAttachmentCap(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{MaxAttachments: 4, MaxAttachmentsPerKey: 1}, map[string]auth.Key{
		"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}},
	})
	be := newSlowStreamer(t, 30, 50*time.Millisecond, "m1") // ~1.5 s
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "m1", func() bool { return e.router.Has("lab/m1") })

	done := startStream(t, e, "alice", "lab/m1")
	time.Sleep(200 * time.Millisecond)
	changed := time.Now()
	be.setModels("m1", "m2")
	waitFor(t, "m2 to appear", func() bool { return e.router.Has("lab/m2") && e.router.Has("lab/m1") })
	if d := time.Since(changed); d > 3*time.Second {
		t.Fatalf("replacing at the cap took %s", d)
	}
	if !streamResult(t, done) {
		t.Fatal("the in-flight stream was cut by the replacement")
	}
}

// When the replacement cannot attach for another reason (here: the edge
// refuses new connections), the old attachment keeps serving the old
// models, and the change applies once attaching works again.
func TestAgentKeepsServingWhenReplacementFails(t *testing.T) {
	shortTimings(t)
	e := newEdge(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"lab/*"}}})
	rl := newRelay(t, e.url.Host)
	via, _ := url.Parse("http://" + rl.ln.Addr().String())
	be := newSlowStreamer(t, 1, time.Millisecond, "m1")
	e.runAgentVia(t, "gpu", be.URL, via)
	waitFor(t, "m1", func() bool { return e.router.Has("lab/m1") })

	rl.refuseNew.Store(true)
	be.setModels("m1", "m2")
	deadline := time.Now().Add(time.Second) // several watch intervals and retries
	for time.Now().Before(deadline) {
		if !e.router.Has("lab/m1") {
			t.Fatal("a failed replacement took the working attachment down")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rl.refuseNew.Store(false)
	waitFor(t, "the change to apply once attaching works", func() bool { return e.router.Has("lab/m2") })
}
