package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
	st, later := steppedStore()
	p.SetStats(st)
	if w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey()); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	later()
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
	st, later := steppedStore()
	p.SetStats(st)
	run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	later()
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
	st, later := steppedStore()
	p.SetStats(st)
	p.BeginDraining()
	run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey())
	later()
	if e := st.Since(0, 1<<20); e.Global.Refusals["draining"] != 1 {
		t.Fatalf("refusals %v", e.Global.Refusals)
	}
}

// Reported usage is clamped to 2^40 per field and a charge can reach the
// response bound, so the scaling must not multiply them as integers.
func TestScaleTokensNoOverflow(t *testing.T) {
	in, out := scaleTokens(1<<40, 1<<40, 1<<26)
	if in < 0 || out < 0 || in+out > 1<<26 || in != out || in < (1<<25)-1 {
		t.Fatalf("scaled to in=%d out=%d", in, out)
	}
	if in, out := scaleTokens(3, 1, 100); in != 3 || out != 1 {
		t.Fatalf("under the charge: %d %d", in, out)
	}
	if in, out := scaleTokens(0, 0, 50); in != 0 || out != 50 {
		t.Fatalf("no split reported: %d %d", in, out)
	}
}

// steppedStore is a stats store whose clock moves only when later is
// called: Since exports complete seconds only.
func steppedStore() (*stats.Store, func()) {
	now := time.Now()
	return stats.New(func() time.Time { return now }), func() { now = now.Add(time.Second) }
}

// A model no route knows is the client's string, not a model: it must
// not become a stats series, or junk names would pin memory and push
// real models into (other).
func TestUnknownModelNotASeries(t *testing.T) {
	p, _ := agentProxy(t, []string{"agent-a-1"}, func(cfg *config.Config) fakeAgents {
		return fakeAgents{"agent-a-1": agentClient(t, cfg, "agent-a-1")}
	})
	st, later := steppedStore()
	p.SetStats(st)
	junk := strings.Repeat("\x1bjunk", 2000)
	if w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"`+strings.ReplaceAll(junk, "\x1b", `\u001b`)+`","messages":[]}`, labKey()); w.Code == 200 {
		t.Fatal("unknown model served")
	}
	later()
	for _, s := range st.Since(0, 1<<20).Series {
		if s.Dim == "model" {
			t.Fatalf("unknown model recorded as series %.40q", s.ID)
		}
	}
}

// An exhausted request-memory budget is not a drain: top must name the
// limit that refused, not tell the operator the daemon is stopping.
func TestBodyBudgetRefusalNamed(t *testing.T) {
	p, _ := agentProxy(t, nil, func(*config.Config) fakeAgents { return fakeAgents{} })
	st, later := steppedStore()
	p.SetStats(st)
	p.budget = newBudget(1)
	if w := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"lab/m","messages":[]}`, labKey()); w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
	later()
	if r := st.Since(0, 1<<20).Global.Refusals; r["body_budget"] != 1 || r["draining"] != 0 {
		t.Fatalf("refusals %v", r)
	}
}
