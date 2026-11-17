package agent_test

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// streamingBackend streams `chunks` SSE events per completion, one every
// `every`, and records the peak number of concurrent streams.
func streamingBackend(t *testing.T, chunks int, every time.Duration) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var active, peak atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"object":"model","id":"m1"}]}`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range chunks {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d %s\"}}]}\n\n", i, strings.Repeat("x", 64))
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	return ts, &peak
}

// 256 concurrent streaming sessions through one agent connection all
// complete, run concurrently rather than queued, and a client that stops
// reading mid-stream does not stall the others: HTTP/2 flow control is
// per stream.
func TestAgent256ConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	const sessions = 256
	e := newEdgeWith(t, config.Agents{MaxConcurrency: sessions + 8}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}, Limits: auth.KeyLimits{ConcurrentRequests: sessions + 8}},
	}, edgeOptions{maxInflight: sessions + 16, extraYAML: `limits:
  global_requests_per_second: 1000
  global_burst: 1000
  preauth_requests_per_second: 1000
  preauth_burst: 1000
`})
	be, peak := streamingBackend(t, 40, 25*time.Millisecond) // ~1 s per stream
	e.runAgent(t, "gpu", be.URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })

	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 0, MaxIdleConnsPerHost: sessions + 8}}
	post := func() (*http.Response, error) {
		req, _ := http.NewRequest("POST", e.url.String()+"/v1/chat/completions", strings.NewReader(chat("lab/m1", true)))
		req.Header.Set("Authorization", "Bearer "+e.keys["alice"])
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	// The stalled client: reads the headers and the first event, then
	// stops reading for the whole test.
	stalled, err := post()
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Body.Close()
	if _, err := bufio.NewReader(stalled.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	var ok, failed atomic.Int64
	var firstErr sync.Once
	for range sessions {
		wg.Go(func() {
			resp, err := post()
			if err != nil {
				failed.Add(1)
				firstErr.Do(func() { t.Errorf("request: %v", err) })
				return
			}
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			done := false
			for sc.Scan() {
				done = done || sc.Text() == "data: [DONE]"
			}
			if resp.StatusCode != 200 || !done {
				failed.Add(1)
				firstErr.Do(func() { t.Errorf("stream: status %d, done %v, err %v", resp.StatusCode, done, sc.Err()) })
				return
			}
			ok.Add(1)
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("%d/%d streams ok in %s, backend peak concurrency %d", ok.Load(), sessions, elapsed.Round(time.Millisecond), peak.Load())
	if ok.Load() != sessions {
		t.Fatalf("%d streams failed", failed.Load())
	}
	// Run concurrently, not one agent slot at a time: 256 one-second
	// streams take about a second in parallel, minutes serialized.
	if p := peak.Load(); p < sessions*3/4 {
		t.Fatalf("backend saw at most %d concurrent streams; want close to %d", p, sessions)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("took %s; streams are not running concurrently", elapsed)
	}
}
