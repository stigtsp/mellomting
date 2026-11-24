package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
	"mellomting/internal/testsupport"
)

// minimalDecisionYAML is the whole configuration SYSTEMONE_PLAN §D11
// promises an operator needs for a decision server, with url filled in.
const minimalDecisionYAML = `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  clef:
    url: %s
models:
  clef-flash:
    type: decision
    servers: [clef]
`

// newParsedProxy builds a proxy the way serve does, from configuration
// text through config.Parse, so defaults applied there (relay on,
// upstream model, decision timeouts) are the ones under test.
func newParsedProxy(t *testing.T, yaml string) (*Proxy, *config.Config) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	router, err := routing.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	clients := make(map[string]*backend.Client, len(cfg.Backends))
	for name, b := range cfg.Backends {
		c, err := backend.New(backend.Options{
			Name: name, Cfg: b, Network: backend.Policy{Mode: "loopback-only"},
			MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(),
			Transport: transportFor(b.BaseURL),
		})
		if err != nil {
			t.Fatal(err)
		}
		clients[name] = c
	}
	p, err := New(cfg, router, clients, testsupport.DiscardLogger(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, cfg
}

// The §D11 block works as written: the call reaches the server with the
// public name as the upstream model, the answer comes back unchanged,
// and a rejection's reason is relayed with no relay setting written.
func TestDecideMinimalConfigEndToEnd(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	p, _ := newParsedProxy(t, fmt.Sprintf(minimalDecisionYAML, f.url))

	rec := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","state":{"pkg":"Acme"},"questions":{"q1":"safe?"}}`, testKey("clef-flash"))
	if rec.Code != 200 {
		t.Fatalf("status = %d (want 200) body = %s", rec.Code, rec.Body.String())
	}
	want := `{"model":"clef-flash","answers":{"q1":"yes"},"usage":{"input_tokens":7,"output_tokens":0}}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s, want the server's answer unchanged", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}

	const reason = "Supply 1-8 questions"
	f.mu.Lock()
	f.failWith = reason
	f.mu.Unlock()
	rec = run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), reason) {
		t.Fatalf("status = %d body = %s, want 400 relaying %q by default", rec.Code, rec.Body.String(), reason)
	}
}

// Without type: decision the model is a generation model, which fails
// safely: /v1/systemone refuses it and never reaches the server.
func TestDecideUntypedModelIsRefused(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	yaml := strings.Replace(fmt.Sprintf(minimalDecisionYAML, f.url), "    type: decision\n", "", 1)
	p, _ := newParsedProxy(t, yaml)

	rec := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not a decision model") {
		t.Fatalf("status = %d body = %s, want 400 not a decision model", rec.Code, rec.Body.String())
	}
	if _, n := f.lastRequest(); n != 0 {
		t.Fatalf("backend received %d requests, want none", n)
	}
}

// §D7: a 503 before any response byte is retried under the budget, and
// the client sees the second attempt's answer; a 400 is the server's
// verdict on the request and is sent once.
func TestDecideRetries(t *testing.T) {
	t.Parallel()
	retrying := func(t *testing.T, status int) (*Proxy, *atomic.Int32) {
		var calls atomic.Int32
		url := pipeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) == 1 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"first attempt"}`))
				return
			}
			_, _ = w.Write([]byte(`{"model":"clef-flash","answers":{},"usage":{"input_tokens":1,"output_tokens":0}}`))
		}))
		cfg := decideConfig(url, "clef-flash")
		cfg.Retry.MaxAttempts = 2
		return newProxyCfg(t, cfg), &calls
	}

	t.Run("503 retried", func(t *testing.T) {
		t.Parallel()
		p, calls := retrying(t, http.StatusServiceUnavailable)
		rec := run(t, p, http.MethodPost, "/v1/systemone", `{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
		if rec.Code != 200 || calls.Load() != 2 {
			t.Fatalf("status = %d after %d calls, want 200 after 2: %s", rec.Code, calls.Load(), rec.Body.String())
		}
	})
	t.Run("400 not retried", func(t *testing.T) {
		t.Parallel()
		p, calls := retrying(t, http.StatusBadRequest)
		rec := run(t, p, http.MethodPost, "/v1/systemone", `{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
		if rec.Code != 400 || calls.Load() != 1 {
			t.Fatalf("status = %d after %d calls, want 400 after 1: %s", rec.Code, calls.Load(), rec.Body.String())
		}
	})
}

// §D10: max_response_bytes bounds a decision answer like any other
// buffered response; an oversized one is not passed through.
func TestDecideResponseIsBounded(t *testing.T) {
	t.Parallel()
	url := pipeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"clef-flash","answers":{"q1":"` + strings.Repeat("x", 4096) + `"}}`))
	}))
	cfg := decideConfig(url, "clef-flash")
	cfg.Server.MaxResponseBytes = 1024
	p := newProxyCfg(t, cfg)

	rec := run(t, p, http.MethodPost, "/v1/systemone", `{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
	if rec.Code == 200 || strings.Contains(rec.Body.String(), "xxxx") {
		t.Fatalf("status = %d, oversized answer passed through (%d bytes)", rec.Code, rec.Body.Len())
	}
}
