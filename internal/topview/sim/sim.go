// Package sim makes up a busy daemon for `mellomting top --simulate`
// and for top's golden frames: keys at their limits, an agent whose
// vLLM is queueing, an SGLang server in cooldown, an Ollama server with
// loaded models and another Mellomting with no metrics. Everything is
// derived from a seed and a clock, so the same seed at the same second
// gives the same frame. Addresses are documentation ranges (RFC 5737,
// RFC 3849).
package sim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/agent"
	"mellomting/internal/backendmetrics"
	"mellomting/internal/inflight"
	"mellomting/internal/stats"
)

type key struct {
	id, name, agent string
	share           float64 // of offered load
	conc            int
	rps             float64
	burst           int
	tph             int64
	models          []string
	remote, ua      string
}

var keys = []key{
	{id: "k-agents", name: "coding_agents", share: 0.42, conc: 8, rps: 10, burst: 20,
		models: []string{"chat-large", "chat-fast"}, remote: "192.0.2.10", ua: "agent-runner/2.1 (host 192.0.2.10)"},
	{id: "k-index", name: "doc_indexer", share: 0.24, conc: 8, rps: 10, burst: 20,
		models: []string{"chat-large"}, remote: "198.51.100.7", ua: "python-httpx/0.28.1"},
	{id: "k-batch", name: "batch_jobs", share: 0.16, rps: 2, burst: 4,
		models: []string{"chat-fast", "local-30b"}, remote: "2001:db8::17", ua: "openai-python/1.99.0"},
	{id: "k-ops", name: "dashboard", share: 0.10, tph: 2_000_000,
		models: []string{"local-30b", "embed-small"}, remote: "192.0.2.44", ua: "curl/8.11.1"},
	{id: "k-lab", name: "notebooks", agent: "", share: 0.08, conc: 4,
		models: []string{"embed-small"}, remote: "198.51.100.23", ua: "Mozilla/5.0 (X11; Linux x86_64)"},
	{id: "k-agent", name: "lab", agent: "lab"},
}

// serverOf is where each model runs.
var serverOf = map[string][]string{
	"chat-large":  {"gpu1", "agent-lab-6"},
	"chat-fast":   {"gpu1"},
	"local-30b":   {"ollama1"},
	"embed-small": {"gpu2", "edge2"},
}

// Sim is a made-up daemon. It is not safe for concurrent use.
type Sim struct {
	store   *stats.Store
	now     time.Time
	started time.Time
	seed    uint64
	clock   func() time.Time
	quota   int64
}

// New fills the five-minute window ending at start. With a clock, each
// Stats call first advances to the clock's time.
func New(seed uint64, start time.Time, clock func() time.Time) *Sim {
	s := &Sim{seed: seed, now: start.Add(-stats.Window).Truncate(time.Second), started: start.Add(-17*time.Hour - 9*time.Minute), clock: clock}
	s.store = stats.New(func() time.Time { return s.now })
	s.quota = 1_610_000
	s.advanceTo(start)
	return s
}

// Stats is the simulated GET /stats.
func (s *Sim) Stats(_ context.Context, since int64) (adminapi.Stats, error) {
	if s.clock != nil {
		s.advanceTo(s.clock())
	}
	return s.Snapshot(since), nil
}

func (s *Sim) advanceTo(t time.Time) {
	for !s.now.After(t.Add(-time.Second)) {
		s.second()
		s.now = s.now.Add(time.Second)
	}
}

// rng is a generator for one second and purpose, so a frame does not
// depend on how many seconds were simulated before it.
func (s *Sim) rng(salt uint64) *rand.Rand {
	return rand.New(rand.NewPCG(s.seed^uint64(s.now.Unix()), salt))
}

// load is the offered requests per second at t: two slow waves and a
// burst every few minutes.
func load(t time.Time) float64 {
	x := float64(t.Unix())
	l := 34 + 12*math.Sin(x/41) + 6*math.Sin(x/11.3)
	if int64(x)%180 < 25 {
		l += 22
	}
	return math.Max(l, 4)
}

func (s *Sim) second() {
	r := s.rng(1)
	l := load(s.now)
	for _, k := range keys {
		if k.share == 0 {
			continue
		}
		n := poisson(r, l*k.share)
		for range n {
			model := k.models[r.IntN(len(k.models))]
			servers := serverOf[model]
			server := servers[r.IntN(len(servers))]
			smp := stats.Sample{KeyID: k.id, KeyName: k.name, Model: model, Server: server, Status: 200,
				BytesIn: int64(2000 + r.IntN(14000)), TTFB: time.Duration(150+r.IntN(1800)) * time.Millisecond}
			smp.TokensIn = int64(400 + r.IntN(3000))
			smp.TokensOut = int64(80 + r.IntN(900))
			smp.BytesOut = smp.TokensOut * 6
			smp.Duration = smp.TTFB + time.Duration(smp.TokensOut*int64(20+r.IntN(40)))*time.Millisecond
			switch {
			case server == "gpu2" && r.Float64() < 0.35:
				smp.Status, smp.ErrorClass, smp.TokensIn, smp.TokensOut, smp.BytesOut = 502, "backend_connect", 0, 0, 120
			case server == "agent-lab-6" && r.Float64() < 0.03:
				smp.Status, smp.ErrorClass, smp.TokensOut = 504, "backend_timeout", 0
			case model == "embed-small":
				smp.TokensOut, smp.BytesOut = 0, 4096
			}
			s.store.Record(smp)
			s.quota += smp.TokensIn + smp.TokensOut
		}
	}
	// The screenshot's case: coding_agents at its concurrency bound.
	for range poisson(r, 3.2+2*math.Sin(float64(s.now.Unix())/23)) {
		s.store.Refuse(stats.KeyConcurrency, "k-agents", "coding_agents", "", "")
	}
	if s.now.Unix()%60 < 12 {
		for range poisson(r, 3) {
			s.store.Refuse(stats.KeyRate, "k-batch", "batch_jobs", "", "")
		}
	}
	if r.Float64() < 0.08 {
		s.store.Refuse(stats.Auth, "", "", "", "")
	}
	if r.Float64() < 0.05 {
		s.store.Record(stats.Sample{KeyID: "k-index", KeyName: "doc_indexer", Model: "chat-large",
			Server: "agent-lab-6", Status: 503, ErrorClass: "queue_timeout", Duration: 30 * time.Second})
	}
	s.store.Sample(s.gauges(r))
}

func (s *Sim) gauges(r *rand.Rand) stats.Gauges {
	l := load(s.now)
	cpan := min(8, 4+int(l/18)+r.IntN(2))
	return stats.Gauges{
		GlobalInflight: s.inflight(),
		Keys: map[string]stats.KeyGauge{
			"k-agents": {Name: "coding_agents", Inflight: 8, AtLimit: true},
			"k-index":  {Name: "doc_indexer", Inflight: cpan, AtLimit: cpan >= 8},
			"k-batch":  {Name: "batch_jobs", Inflight: 1 + r.IntN(3)},
			"k-lab":    {Name: "notebooks", Inflight: r.IntN(3)},
		},
		Servers: map[string]stats.ServerGauge{
			"gpu1":        {Admitted: s.gpu1(), HasMetrics: true, Running: s.gpu1(), KVPercent: 41 + 20*math.Sin(float64(s.now.Unix())/37)},
			"agent-lab-6": {Admitted: 24, HasMetrics: true, Running: 6, Waiting: s.labWaiting(), KVPercent: 94},
			"gpu2":        {Admitted: 1, HasMetrics: true, Running: 1, KVPercent: 12},
			"ollama1":     {Admitted: 2 + r.IntN(2)},
			"edge2":       {Admitted: r.IntN(3)},
		},
	}
}

// dropped scripts a disk stall: for half a minute of every five, the
// accounting writer drops records.
func dropped(t time.Time) int64 {
	if c := t.Unix() % 300; c >= 200 && c < 230 {
		return (c - 199) * 7
	}
	return 0
}

func (s *Sim) gpu1() int       { return 10 + int(load(s.now)/6) }
func (s *Sim) labWaiting() int { return 16 + int(s.now.Unix()%5) }
func (s *Sim) inflight() int   { return 8 + 7 + 3 + 1 + s.gpu1() + 2 }

func poisson(r *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	// Knuth: fine for the small means used here.
	lim, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= r.Float64()
		if p <= lim {
			return k
		}
		k++
	}
}

// Snapshot is the reply at the current simulated second.
func (s *Sim) Snapshot(since int64) adminapi.Stats {
	now := s.now
	r := s.rng(2)
	st := adminapi.Stats{
		NowMillis:     now.UnixMilli(),
		StartedMillis: s.started.UnixMilli(),
		Global: adminapi.GlobalInfo{MaxInflight: 256, Inflight: s.inflight(), RPS: 200, Burst: 400,
			Accounting: adminapi.AccountingInfo{Enabled: true, Dropped: dropped(now)}},
		Stats: s.store.Since(since, adminapi.MaxStatsBytes),
	}
	hour := now.Truncate(time.Hour).Add(time.Hour)
	day := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	inflightOf := map[string]int{"k-agents": 8, "k-index": min(8, 4+int(load(now)/18)), "k-batch": 2, "k-lab": 1}
	for _, k := range keys {
		ki := adminapi.KeyInfo{ID: k.id, Name: k.name, Agent: k.agent, ConcurrentRequests: k.conc,
			RequestsPerSecond: k.rps, Burst: k.burst, Inflight: inflightOf[k.id], TokensPerHour: k.tph}
		if k.tph > 0 {
			ki.HourUsed = k.tph*8/10 + s.quota%(k.tph/10)
			ki.HourResetsMillis, ki.DayResetsMillis = hour.UnixMilli(), day.UnixMilli()
		}
		st.Keys = append(st.Keys, ki)
	}
	wave := float64(now.Unix())
	st.Servers = []adminapi.ServerInfo{
		{Name: "edge2", Kind: "configured", Models: []string{"embed-small"}, Admitted: 1, MaxConcurrency: 16, QueueSize: 64},
		{Name: "gpu1", Kind: "configured", Models: []string{"chat-fast", "chat-large"},
			Admitted: s.gpu1(), MaxConcurrency: 32, QueueSize: 64,
			Metrics: &backendmetrics.Reading{Engine: "vllm", HasQueue: true, Running: float64(s.gpu1()),
				KVPercent: 41 + 20*math.Sin(wave/37), PromptTokens: 9.1e8, GenerationTokens: 2.2e8, At: now}},
		{Name: "gpu2", Kind: "configured", Models: []string{"embed-small"}, Admitted: 1, MaxConcurrency: 8, QueueSize: 16,
			Health: []adminapi.HealthInfo{{Backend: "auto-gpu2-1f2e3d4c5b6a", Model: "embed-small",
				CooldownUntilMillis: now.Add(time.Duration(40-now.Unix()%40) * time.Second).UnixMilli()}},
			Metrics: &backendmetrics.Reading{Engine: "sglang", HasQueue: true, Running: 1, KVPercent: 12, At: now}},
		{Name: "ollama1", Kind: "configured", Models: []string{"local-30b"}, Admitted: 2 + r.IntN(2), MaxConcurrency: 4, QueueSize: 16,
			Metrics: &backendmetrics.Reading{Engine: "ollama", At: now, Loaded: []backendmetrics.LoadedModel{
				{Name: "local-30b", VRAMBytes: 19e9, ExpiresAt: now.Add(4 * time.Minute)},
				{Name: "nomic-embed-text:latest", VRAMBytes: 6e8, ExpiresAt: now.Add(90 * time.Second)}}}},
	}
	st.Agents = []agent.Record{{
		Name: "agent-lab-6", KeyName: "lab", Namespace: "lab", Remote: "203.0.113.50:51834", AgeMillis: 3*3600_000 + 12*60_000,
		State: "serving", Models: []agent.Model{{Name: "chat-large", ContextLength: 131072}},
		Admitted: 24, MaxConcurrency: 24, Streams: 24, Requests: 18342, Failures: 3,
		Metrics: &backendmetrics.Reading{Engine: "vllm", HasQueue: true, Running: 6, Waiting: float64(s.labWaiting()), KVPercent: 94,
			PromptTokens: 3.3e8, GenerationTokens: 7.1e7, Preemptions: 12, At: now},
	}}
	st.Requests = s.requests(r)
	return st
}

// requests makes up the live table: the harness key's eight, the
// agent's 24 mostly waiting on vLLM's queue, and a few others.
func (s *Sim) requests(r *rand.Rand) []inflight.Record {
	var out []inflight.Record
	add := func(k key, model, server string, phase inflight.Phase, age time.Duration) {
		rec := inflight.Record{RequestID: fmt.Sprintf("req-%04x%04x", s.now.Unix()&0xffff, len(out)), KeyID: k.id, KeyName: k.name,
			Remote: fmt.Sprintf("%s:%d", bracket(k.remote), 40000+r.IntN(20000)), UserAgent: k.ua, Endpoint: "/v1/chat/completions",
			Model: model, Backend: server, Phase: phase, AgeMillis: age.Milliseconds(), BytesIn: int64(2000 + r.IntN(30000))}
		rec.PhaseMillis = rec.AgeMillis
		switch phase {
		case inflight.PhaseStreaming:
			rec.TTFBMillis = int64(300 + r.IntN(2000))
			rec.PhaseMillis = rec.AgeMillis - rec.TTFBMillis
			rec.BytesOut = int64(r.IntN(40000))
		case inflight.PhaseWaiting:
			rec.PhaseMillis = rec.AgeMillis - int64(r.IntN(200))
		}
		out = append(out, rec)
	}
	for i := range 8 {
		ph := inflight.PhaseStreaming
		if i%3 == 0 {
			ph = inflight.PhaseWaiting
		}
		add(keys[0], keys[0].models[i%2], "gpu1", ph, time.Duration(2+r.IntN(60))*time.Second)
	}
	for i := range 6 {
		ph := inflight.PhaseWaiting
		if i < 2 {
			ph = inflight.PhaseStreaming
		}
		add(keys[1], keys[1].models[0], "agent-lab-6", ph, time.Duration(20+r.IntN(150))*time.Second)
	}
	add(keys[2], "local-30b", "ollama1", inflight.PhaseStreaming, 4*time.Second)
	add(keys[2], "chat-fast", "gpu1", inflight.PhaseQueued, 1200*time.Millisecond)
	add(keys[4], "embed-small", "edge2", inflight.PhaseSending, 300*time.Millisecond)
	add(keys[3], "local-30b", "ollama1", inflight.PhaseReading, 80*time.Millisecond)
	return out
}

func bracket(ip string) string {
	for _, c := range ip {
		if c == ':' {
			return "[" + ip + "]"
		}
	}
	return ip
}

// BackendServer maps the generated backend names in the simulated
// accounting log to their servers, as top builds it from configuration.
var BackendServer = map[string]string{
	"auto-gpu1-6a1d0c3e9b27":    "gpu1",
	"auto-gpu1-0f9e8d7c6b5a":    "gpu1",
	"auto-gpu2-1f2e3d4c5b6a":    "gpu2",
	"auto-ollama1-3c4d5e6f7a8b": "ollama1",
	"auto-edge2-9a8b7c6d5e4f":   "edge2",
}

var usageBackend = map[string][]string{
	"chat-large":  {"auto-gpu1-6a1d0c3e9b27", "agent-lab-3", "agent-lab-6"},
	"chat-fast":   {"auto-gpu1-0f9e8d7c6b5a"},
	"local-30b":   {"auto-ollama1-3c4d5e6f7a8b"},
	"embed-small": {"auto-gpu2-1f2e3d4c5b6a", "auto-edge2-9a8b7c6d5e4f"},
}

// WriteUsage writes seven days of accounting records ending at the
// current simulated second, busier in working hours, in the daemon's
// usage.jsonl format.
func (s *Sim) WriteUsage(w io.Writer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	r := rand.New(rand.NewPCG(s.seed, 99))
	end := s.now
	for t := end.Add(-7 * 24 * time.Hour).Truncate(time.Hour); t.Before(end); t = t.Add(time.Hour) {
		busy := 0.3 + 0.7*math.Max(0, math.Sin(float64(t.Hour()-6)/24*2*math.Pi))
		for _, k := range keys {
			if k.share == 0 {
				continue
			}
			for range poisson(r, 30*busy*k.share) {
				model := k.models[r.IntN(len(k.models))]
				bs := usageBackend[model]
				in, out := int64(400+r.IntN(3000)), int64(80+r.IntN(900))
				status := "exact"
				if r.Float64() < 0.01 {
					status = "unknown"
				}
				rec := map[string]any{
					"time": t.Add(time.Duration(r.IntN(3600)) * time.Second).Format(time.RFC3339Nano), "request_id": "sim",
					"key_id": k.id, "model": model, "backend": bs[r.IntN(len(bs))], "endpoint": "/v1/chat/completions",
					"status": 200, "input_tokens": in, "output_tokens": out, "total_tokens": in + out,
					"cached_tokens": in / 4, "usage_status": status, "charged_tokens": in + out,
				}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
		}
	}
	return bw.Flush()
}
