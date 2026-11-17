package agent_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// lyingBackend advertises ids and claims an absurd token usage for every
// completion.
func lyingBackend(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[`)
			for i, id := range ids {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"object":"model","id":%q}`, id)
			}
			fmt.Fprint(w, `]}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Retry-After", "99999999")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1099511627776,"total_tokens":1099511627777}}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// A malicious agent cannot exhaust a client key's token budget by
// claiming impossible usage: the charge is bounded by the bytes that
// moved (PLAN §104.6).
func TestAgentUsageClaimIsBounded(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}, Limits: auth.KeyLimits{TokensPerHour: 100_000}},
	})
	e.runAgent(t, "gpu", lyingBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	for i := range 3 {
		if code, body := e.do(t, "POST", "/v1/chat/completions", "alice", chat("lab/m1", false)); code != 200 {
			t.Fatalf("request %d: %d %s (a lying agent exhausted the key's budget)", i, code, body)
		}
	}
}

// An agent's response headers are bounded: no HTML content type, no
// day-long Retry-After.
func TestAgentResponseHeadersSanitized(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	e.runAgent(t, "gpu", lyingBackend(t, "m1").URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/m1") })
	req, _ := http.NewRequest("POST", e.url.String()+"/v1/chat/completions", stringsReader(chat("lab/m1", false)))
	req.Header.Set("Authorization", "Bearer "+e.keys["alice"])
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct == "text/html" {
		t.Fatalf("agent-chosen Content-Type relayed: %q", ct)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "99999999" {
		t.Fatalf("agent-chosen Retry-After relayed: %q", ra)
	}
}

// A * key cannot register names inside another namespace, and a second
// key in the same namespace cannot join (and so intercept) the first
// key's model.
func TestAgentCannotSquatNames(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"star": {Agent: "*"},
		"gpu":  {Agent: "lab"},
		"gpu2": {Agent: "lab"},
	})
	e.runAgent(t, "star", lyingBackend(t, "lab/m1", "lab/new", "plain").URL)
	waitFor(t, "star's plain model", func() bool { return e.router.Has("plain") })
	if e.router.Has("lab/m1") || e.router.Has("lab/new") {
		t.Fatal("a * agent registered a name inside the lab/ namespace")
	}

	e.runAgent(t, "gpu", lyingBackend(t, "m1").URL)
	waitFor(t, "lab/m1", func() bool { return e.router.Has("lab/m1") })
	// gpu2's only model is owned by gpu, so its attachments register
	// nothing and close; meanwhile lab/m1 must keep routing to gpu.
	// Count gpu2's model-list requests: the agent's own probe is one, so
	// a second means the edge discovered through a real attachment.
	var listed atomic.Int32
	lying := lyingBackend(t, "m1")
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			listed.Add(1)
		}
		proxyTo(w, r, lying.URL)
	}))
	t.Cleanup(counting.Close)
	e.runAgent(t, "gpu2", counting.URL)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tg, err := e.router.Select("lab/m1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Backend != "agent-gpu-2" {
			t.Fatalf("lab/m1 routed to %s, not the key that registered it", tg.Backend)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if listed.Load() < 2 {
		t.Fatal("gpu2 never attached; the test proved nothing")
	}
}

// proxyTo relays one request to base, for wrapping a test server.
func proxyTo(w http.ResponseWriter, r *http.Request, base string) {
	req, _ := http.NewRequest(r.Method, base+r.URL.Path, r.Body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func TestAgentAttachmentLimitPerKey(t *testing.T) {
	e := newEdge(t, config.Agents{MaxAttachments: 4, MaxAttachmentsPerKey: 1}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"other": {Agent: "exp"},
	})
	e.runAgent(t, "gpu", lyingBackend(t, "m1").URL)
	waitFor(t, "first attachment", func() bool { return e.router.Has("lab/m1") })
	if code := attachStatus(t, e, e.keys["gpu"]); code != http.StatusServiceUnavailable {
		t.Fatalf("second attachment of one key: got %d, want 503", code)
	}
	if code := attachStatus(t, e, e.keys["other"]); code != http.StatusSwitchingProtocols {
		t.Fatalf("another key must still attach: got %d, want 101", code)
	}
}
