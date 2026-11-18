package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"mellomting/internal/config"
)

// silentTransport never answers: it holds every request until its
// context ends, like an attached agent whose server hangs.
type silentTransport struct{}

func (silentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// answeringTransport answers at once.
type answeringTransport struct{}

func (answeringTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")), Request: req,
	}, nil
}

func injectedClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := New(Options{
		Name: "agent-test-1",
		Cfg: config.Backend{
			BaseURL: "http://agent.invalid", MaxConcurrency: 1, QueueSize: 1,
			QueueTimeout:  config.Duration(time.Second),
			HeaderTimeout: config.Duration(150 * time.Millisecond), RequestTimeout: config.Duration(400 * time.Millisecond),
			StreamIdleTimeout: config.Duration(time.Second),
		},
		Network:          Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
		Transport:        rt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An injected transport (an attached agent) gets the same header bounds
// as a dialing one: header_timeout for streams, request_timeout
// otherwise. Without them a silent agent would hold its admission slot
// until the client gave up.
func TestInjectedTransportHeaderTimeouts(t *testing.T) {
	for _, c := range []struct {
		stream bool
		want   error
		within time.Duration
	}{
		{true, ErrHeaderTimeout, time.Second},
		{false, ErrTimeout, 2 * time.Second},
	} {
		client := injectedClient(t, silentTransport{})
		start := time.Now()
		_, err := client.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: c.stream})
		if !errors.Is(err, c.want) {
			t.Fatalf("stream=%v: got %v, want %v", c.stream, err, c.want)
		}
		if d := time.Since(start); d > c.within {
			t.Fatalf("stream=%v: took %s, want under %s", c.stream, d, c.within)
		}
		if client.Admitted() != 0 {
			t.Fatalf("stream=%v: admission slot still held after the timeout", c.stream)
		}
	}
}

// A prompt answer is unaffected, and reading the stream after the header
// bound has passed still works.
func TestInjectedTransportAnswer(t *testing.T) {
	client := injectedClient(t, answeringTransport{})
	res, err := client.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	time.Sleep(200 * time.Millisecond) // past header_timeout
	b, err := io.ReadAll(res.Body)
	if err != nil || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("body after header bound: %q %v", b, err)
	}
}

type probeRT func(*http.Request) (*http.Response, error)

func (f probeRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Probe reads a server's own endpoints (metrics) outside admission,
// with the configured credential and a bounded body.
func TestProbe(t *testing.T) {
	var got *http.Request
	c, err := New(Options{
		Name: "b1", Cfg: config.Backend{BaseURL: "http://127.0.0.1:8000", UpstreamModel: "m", MaxConcurrency: 1,
			ConnectTimeout: config.Duration(time.Second), HeaderTimeout: config.Duration(time.Second), RequestTimeout: config.Duration(time.Second)},
		Network: Policy{Mode: "loopback-only"}, MaxResponseBytes: 1 << 20,
		Transport: probeRT(func(r *http.Request) (*http.Response, error) {
			got = r
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("0123456789")), Request: r}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := c.Probe(context.Background(), "/metrics", 4)
	if err != nil || status != 200 || string(body) != "0123" {
		t.Fatalf("status %d body %q err %v", status, body, err)
	}
	if got.Method != http.MethodGet || got.URL.String() != "http://127.0.0.1:8000/metrics" {
		t.Fatalf("request %s %s", got.Method, got.URL)
	}
	if c.Admitted() != 0 {
		t.Fatal("probe took an admission slot")
	}
}
