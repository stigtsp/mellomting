package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/testsupport"
)

// TestModelsOnOneServerShareItsAdmission: two public models served by
// one engine are admitted against that engine's capacity once, not once
// per model. Separate pools would let the engine see twice its cap, and
// the excess would wait in its own queue, where queue_size does not
// bound it and a client disconnect does not drain it.
func TestModelsOnOneServerShareItsAdmission(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	hold := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	})
	local := httptest.NewServer(hold)
	defer local.Close()
	other := httptest.NewServer(hold)
	defer other.Close()
	defer close(release)

	cfg, err := config.Parse(fmt.Appendf(nil, `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local:
    url: %s
  other:
    url: %s
models:
  model-a:
    servers: [local]
  model-b:
    servers: [local]
  model-c:
    servers: [other]
`, local.URL, other.URL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, b := range cfg.Backends {
		b.MaxConcurrency = 1
		b.QueueSize = 0
		cfg.Backends[name] = b
	}
	clients, err := newBackendClients(cfg, backend.Policy{Mode: "loopback-only"}, nil, testsupport.DiscardLogger())
	if err != nil {
		t.Fatalf("newBackendClients: %v", err)
	}
	client := func(model string) *backend.Client {
		return clients[cfg.Models[model].Backends[0].Name]
	}
	req := backend.Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true}

	res, err := client("model-a").Forward(context.Background(), req)
	if err != nil {
		t.Fatalf("model-a: %v", err)
	}
	defer res.Close()

	if _, err := client("model-b").Forward(context.Background(), req); !errors.Is(err, backend.ErrQueueFull) {
		t.Fatalf("model-b on the same server: err = %v, want ErrQueueFull", err)
	}
	// Another server is another engine, with capacity of its own.
	res2, err := client("model-c").Forward(context.Background(), req)
	if err != nil {
		t.Fatalf("model-c on another server: %v", err)
	}
	res2.Close()
}
