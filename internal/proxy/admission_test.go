package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/config"
)

// slowStream is a backend that takes prefillDelay to produce its first
// response byte and records how many requests it served at once. It
// stands in for a local inference server: the first header waits on
// scheduler admission and prefill, not on the network.
type slowStream struct {
	prefillDelay time.Duration
	active       atomic.Int64
	peak         atomic.Int64
	served       atomic.Int64
}

func (s *slowStream) handler(w http.ResponseWriter, _ *http.Request) {
	n := s.active.Add(1)
	for {
		peak := s.peak.Load()
		if n <= peak || s.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer s.active.Add(-1)
	s.served.Add(1)

	time.Sleep(s.prefillDelay)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"))
}

// admissionConfig builds a single-backend config with explicit admission
// and timeout settings, so each test states the property it depends on
// rather than inheriting the shipped defaults.
func admissionConfig(serverURL string, conc, queue int, queueTimeout, headerTimeout time.Duration) *config.Config {
	cfg := testConfig(serverURL)
	b := cfg.Backends["b1"]
	b.MaxConcurrency = conc
	b.QueueSize = queue
	b.QueueTimeout = config.Duration(queueTimeout)
	b.HeaderTimeout = config.Duration(headerTimeout)
	cfg.Backends["b1"] = b
	return cfg
}

// runStreams dispatches n concurrent streaming chat requests and returns
// their recorders in completion-independent order.
func runStreams(t *testing.T, p *Proxy, n int) []*httptest.ResponseRecorder {
	t.Helper()
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			recs[i] = run(t, p, http.MethodPost, "/v1/chat/completions",
				`{"model":"gen-1","stream":true}`, testKey())
		})
	}
	wg.Wait()
	return recs
}

// TestBackendConcurrencyBoundsUpstreamRequests: the admission cap is what
// reaches the backend. Excess work waits in Mellomting's bounded queue,
// where queue_size bounds it and a disconnect drains it, instead of being
// pushed into the backend's private queue where neither is true.
func TestBackendConcurrencyBoundsUpstreamRequests(t *testing.T) {
	t.Parallel()

	for _, conc := range []int{1, 2} {
		t.Run(fmt.Sprintf("concurrency=%d", conc), func(t *testing.T) {
			t.Parallel()
			s := &slowStream{prefillDelay: 100 * time.Millisecond}
			f := newFakeVLLM(t, s.handler)
			cfg := admissionConfig(f.server.URL, conc, 8, 10*time.Second, 5*time.Second)
			p := newProxyCfg(t, cfg)

			const requests = 6
			for i, rec := range runStreams(t, p, requests) {
				if rec.Code != http.StatusOK {
					t.Fatalf("request %d: status = %d, want 200 (body %s)", i, rec.Code, rec.Body.String())
				}
			}
			if got := s.served.Load(); got != requests {
				t.Fatalf("backend served %d requests, want %d", got, requests)
			}
			if got := s.peak.Load(); got != int64(conc) {
				t.Fatalf("peak upstream concurrency = %d, want exactly max_concurrency (%d)", got, conc)
			}
		})
	}
}

// TestQueueWaitIsNotChargedToHeaderTimeout is the second report's core
// property. header_timeout bounds the backend's first response byte; it
// must start when the request is forwarded, never when it is accepted.
// Charging admission wait to it turns a healthy queue into a wave of
// 504s — the reported failure, where every one landed at 30.0s.
func TestQueueWaitIsNotChargedToHeaderTimeout(t *testing.T) {
	t.Parallel()

	const (
		prefill  = 100 * time.Millisecond
		requests = 10
		// Six times one prefill, well under all ten: a header clock
		// that started at admission fails, one that starts at
		// forwarding passes with enough slack that a loaded machine
		// does not decide the outcome.
		headerTimeout = 600 * time.Millisecond
	)
	s := &slowStream{prefillDelay: prefill}
	f := newFakeVLLM(t, s.handler)
	// The queue holds every waiter: this test is about the header
	// clock, so nothing may be refused for want of a queue slot.
	cfg := admissionConfig(f.server.URL, 1, requests, 10*time.Second, headerTimeout)
	p := newProxyCfg(t, cfg)

	start := time.Now()
	recs := runStreams(t, p, requests)
	elapsed := time.Since(start)

	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 — a queued request was failed as a backend timeout (body %s)",
				i, rec.Code, rec.Body.String())
		}
	}
	// If the requests had not serialized, the test would prove nothing:
	// none of them would have waited longer than header_timeout.
	if elapsed <= headerTimeout {
		t.Fatalf("requests finished in %v, within header_timeout (%v): they did not queue, so nothing was proven",
			elapsed, headerTimeout)
	}
}

// TestQueueExhaustionAndHeaderTimeoutDifferInClass keeps the two
// failures an operator must tell apart distinct: one says Mellomting is
// holding more work than it agreed to, the other says the backend went
// quiet. Collapsing them would hide which knob to turn.
func TestQueueExhaustionAndHeaderTimeoutDifferInClass(t *testing.T) {
	t.Parallel()

	t.Run("queue exhaustion", func(t *testing.T) {
		t.Parallel()
		s := &slowStream{prefillDelay: 400 * time.Millisecond}
		f := newFakeVLLM(t, s.handler)
		// One slot, no queue: the second concurrent request has
		// nowhere to wait.
		cfg := admissionConfig(f.server.URL, 1, 0, time.Second, 5*time.Second)
		var buf bytes.Buffer
		p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

		var overloaded atomic.Int64
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if run(t, p, http.MethodPost, "/v1/chat/completions",
					`{"model":"gen-1","stream":true}`, testKey()).Code == http.StatusServiceUnavailable {
					overloaded.Add(1)
				}
			})
		}
		wg.Wait()

		if overloaded.Load() == 0 {
			t.Fatalf("no request was refused with 503 despite a full queue: %s", buf.String())
		}
		if !hasClass(t, &buf, "queue_full") {
			t.Fatalf("queue exhaustion was not classified queue_full: %s", buf.String())
		}
		if hasClass(t, &buf, "backend_timeout") {
			t.Fatalf("queue exhaustion was reported as a backend timeout: %s", buf.String())
		}
	})

	t.Run("header timeout", func(t *testing.T) {
		t.Parallel()
		s := &slowStream{prefillDelay: time.Second}
		f := newFakeVLLM(t, s.handler)
		cfg := admissionConfig(f.server.URL, 4, 8, 10*time.Second, 100*time.Millisecond)
		var buf bytes.Buffer
		p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

		rec := run(t, p, http.MethodPost, "/v1/chat/completions",
			`{"model":"gen-1","stream":true}`, testKey())

		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
		}
		if !hasClass(t, &buf, "backend_timeout") {
			t.Fatalf("header timeout was not classified backend_timeout: %s", buf.String())
		}
		if hasClass(t, &buf, "queue_full") {
			t.Fatalf("header timeout was reported as queue exhaustion: %s", buf.String())
		}
	})
}

// TestCancelledRequestReleasesAdmission: a client that goes away while
// queued must free its slot immediately. A deep queue is only safe if
// abandoned work leaves it — otherwise an agent's retries would fill it
// with requests nobody is waiting for.
func TestCancelledRequestReleasesAdmission(t *testing.T) {
	t.Parallel()

	// The holder occupies the slot for a full second, so the window in
	// which the second request is provably still queued is wide enough
	// that a slow machine cannot close it.
	s := &slowStream{prefillDelay: time.Second}
	f := newFakeVLLM(t, s.handler)
	cfg := admissionConfig(f.server.URL, 1, 8, 10*time.Second, 5*time.Second)
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

	// Hold the single concurrency slot, and wait until the backend
	// confirms it: starting the second request before the first is
	// admitted would let it take the slot and leave nothing queued.
	var holder sync.WaitGroup
	holder.Go(func() {
		run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1","stream":true}`, testKey())
	})
	deadline := time.Now().Add(2 * time.Second)
	for s.active.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the holding request never reached the backend")
		}
		time.Sleep(time.Millisecond)
	}

	// Queue a request behind it, then hang up while it waits.
	ctx, cancel := context.WithCancel(context.Background())
	var queued sync.WaitGroup
	queued.Go(func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gen-1","stream":true}`)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		p.ChatCompletions(&Req{
			W: httptest.NewRecorder(), R: r, Key: testKey(),
			RequestID: "req_cancelled", Remote: "127.0.0.1",
		})
	})
	time.Sleep(50 * time.Millisecond)
	cancel()
	queued.Wait()
	holder.Wait()

	if !hasClass(t, &buf, "client_canceled") {
		t.Fatalf("a client that hung up while queued was not recorded as client_canceled: %s", buf.String())
	}
	// The abandoned request never reached the backend, and the slot it
	// was waiting for is free: a later request is served normally.
	if got := s.served.Load(); got != 1 {
		t.Fatalf("backend served %d requests, want 1 — the cancelled request was forwarded", got)
	}
	if rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey()); rec.Code != http.StatusOK {
		t.Fatalf("after cancellation the slot was not released: status = %d", rec.Code)
	}
}

// hasClass reports whether any request line in a JSON log buffer carries
// the given error_class.
func hasClass(t *testing.T, buf *bytes.Buffer, class string) bool {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg   string `json:"msg"`
			Class string `json:"error_class"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec.Msg == "request" && rec.Class == class {
			return true
		}
	}
	return false
}

// TestDrainReleasesQueuedRequests: queue_timeout is far longer than the
// shutdown grace period, so a queued request that shutdown does not
// release is severed mid-wait — its handler never reaches the deferred
// accounting call, and its client gets a connection reset instead of the
// sanitized 503 draining exists to produce.
func TestDrainReleasesQueuedRequests(t *testing.T) {
	t.Parallel()

	s := &slowStream{prefillDelay: time.Second}
	f := newFakeVLLM(t, s.handler)
	// A queue wait no test would sit through: only the drain can end it.
	cfg := admissionConfig(f.server.URL, 1, 8, time.Hour, 5*time.Second)
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

	var holder sync.WaitGroup
	holder.Go(func() {
		run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1","stream":true}`, testKey())
	})
	deadline := time.Now().Add(2 * time.Second)
	for s.active.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the holding request never reached the backend")
		}
		time.Sleep(time.Millisecond)
	}

	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		queued <- run(t, p, http.MethodPost, "/v1/chat/completions",
			`{"model":"gen-1","stream":true}`, testKey())
	}()
	time.Sleep(50 * time.Millisecond)

	p.BeginDraining()

	select {
	case rec := <-queued:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("drained request: status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draining did not release the queued request; it is still waiting out queue_timeout")
	}
	holder.Wait()

	if !hasClass(t, &buf, "overload") {
		t.Fatalf("the drained request was not accounted as a drain: %s", buf.String())
	}
}
