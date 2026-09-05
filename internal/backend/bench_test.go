package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mellomting/internal/config"
)

// BenchmarkForwardNonStream measures the raw non-stream response
// forwarding path against a loopback fake backend (PLAN §87): request
// egress, body buffering, and the returned Result.
func BenchmarkForwardNonStream(b *testing.B) {
	const respBody = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer ts.Close()

	opts := Options{
		Name: "bench",
		Cfg: config.Backend{
			BaseURL:           ts.URL,
			MaxConcurrency:    1024,
			QueueSize:         1024,
			QueueTimeout:      config.Duration(time.Second),
			ConnectTimeout:    config.Duration(time.Second),
			HeaderTimeout:     config.Duration(2 * time.Second),
			RequestTimeout:    config.Duration(5 * time.Second),
			StreamIdleTimeout: config.Duration(time.Second),
		},
		Network:          Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
	}
	c, err := New(opts)
	if err != nil {
		b.Fatal(err)
	}
	req := Request{
		Method: "POST",
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`),
	}
	b.ReportAllocs()
	for b.Loop() {
		res, err := c.Forward(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if res.BodyBytes == nil {
			b.Fatal("no body")
		}
	}
}
