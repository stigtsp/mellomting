package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/stats"
	"mellomting/internal/testsupport"
)

func series(e stats.Export, dim, id string) *stats.SeriesExport {
	for i := range e.Series {
		if e.Series[i].Dim == dim && e.Series[i].ID == id {
			return &e.Series[i]
		}
	}
	return nil
}

func TestRequestRecordedInStats(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-1": agentClient(t, cfg, "agent-a-1")}
	})
	st := stats.New(nil)
	p.SetStats(st)
	if w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey()); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	e := st.Since(0, 1<<20)
	b := e.Global.Buckets[0]
	if b.OK != 1 || b.TokensIn != 1 || b.TokensOut != 1 || b.BytesOut == 0 {
		t.Fatalf("global bucket %+v", b)
	}
	for _, want := range [][2]string{{"key", "K1"}, {"model", "lab/m"}, {"server", "agent-a-1"}} {
		if series(e, want[0], want[1]) == nil {
			t.Errorf("no %s series %q", want[0], want[1])
		}
	}
}

// An attached agent's usage claim is scaled to what accounting charged,
// so a lying agent cannot inflate the graphs (PLAN §104.6).
func TestUntrustedTokensScaledToCharge(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(cfg *config.Config) fakeAgents {
		c, err := backend.New(backend.Options{
			Name: "agent-a-1", Cfg: config.AgentBackend(cfg.Agents), Network: backend.Policy{Mode: "loopback-only"},
			MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(), Untrusted: true,
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":900000000,"completion_tokens":100000000,"total_tokens":1000000000}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return fakeAgents{"agent-a-1": c}
	})
	st := stats.New(nil)
	p.SetStats(st)
	run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	b := st.Since(0, 1<<20).Global.Buckets[0]
	if moved := b.BytesIn + b.BytesOut; b.TokensIn+b.TokensOut > moved || b.TokensIn+b.TokensOut == 0 {
		t.Fatalf("tokens in=%d out=%d exceed the %d bytes that moved", b.TokensIn, b.TokensOut, moved)
	}
	if b.TokensIn <= b.TokensOut {
		t.Fatalf("scaling lost the reported split: in=%d out=%d", b.TokensIn, b.TokensOut)
	}
}

func TestDrainingRecordedAsRefusal(t *testing.T) {
	p, _ := agentProxy(t, nil, func(*config.Config) fakeAgents { return fakeAgents{} })
	st := stats.New(nil)
	p.SetStats(st)
	p.BeginDraining()
	run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	if e := st.Since(0, 1<<20); e.Global.Refusals["draining"] != 1 {
		t.Fatalf("refusals %v", e.Global.Refusals)
	}
}
