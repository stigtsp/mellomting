package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"mellomting/internal/testsupport"
)

// A client that hangs up mid-stream makes ReverseProxy abort with
// http.ErrAbortHandler. The in-flight count must still drop back: the
// health watcher ignores failed probes while it is above zero, so a
// leaked count would keep an agent attached to a dead server.
func TestRelayCountsAbortedStreams(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 200; i++ {
			if _, err := io.WriteString(w, "data: {\"choices\":[]}\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer be.Close()
	u, _ := url.Parse(be.URL)
	st := &stats{}
	relay := httptest.NewServer(backendHandler(Options{Backend: u, Log: testsupport.DiscardLogger()}, st))
	defer relay.Close()

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relay.URL+"/v1/chat/completions", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf) // first chunk arrives, then hang up
		cancel()
		resp.Body.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.active.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("active = %d after every client hung up, want 0", st.active.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := st.requests.Load(); got != 5 {
		t.Fatalf("requests = %d, want 5", got)
	}
}
