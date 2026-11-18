package main

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/adminapi"
	"mellomting/internal/agent"
	"mellomting/internal/backend"
	"mellomting/internal/backendmetrics"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/inflight"
	"mellomting/internal/routing"
	"mellomting/internal/stats"
)

// statsSource assembles the admin socket's /stats reply from the
// daemon's parts. Every field it reads is already kept for serving
// requests; nothing here is on the request path.
type statsSource struct {
	cfg     *config.Config
	started time.Time
	store   *stats.Store
	api     *httpapi.Server
	clients map[string]*backend.Client
	router  *routing.Router
	quota   *accounting.Quota
	acc     *accounting.Writer
	live    *inflight.Registry
	agents  *agent.Registry // nil when agents are disabled
	// metrics returns the latest backend reports by server name; nil
	// when nothing is polled.
	metrics func() map[string]backendmetrics.Reading
}

func (s *statsSource) Stats(since int64) adminapi.Stats {
	now := time.Now()
	out := adminapi.Stats{
		NowMillis:     now.UnixMilli(),
		StartedMillis: s.started.UnixMilli(),
		Global: adminapi.GlobalInfo{
			MaxInflight: s.cfg.Server.MaxInflightRequests,
			Inflight:    s.api.Inflight(),
			RPS:         s.cfg.Limits.GlobalRequestsPerSecond,
			Burst:       s.cfg.Limits.GlobalBurst,
			Accounting:  adminapi.AccountingInfo{Enabled: s.acc != nil},
		},
		Stats:    s.store.Since(since, adminapi.MaxStatsBytes),
		Keys:     s.keys(now),
		Servers:  s.servers(),
		Requests: s.live.Snapshot(),
	}
	if s.acc != nil {
		out.Global.Accounting.Dropped = s.acc.Dropped()
	}
	if s.agents != nil {
		out.Agents = s.agents.Snapshot()
	}
	return out
}

func (s *statsSource) keys(now time.Time) []adminapi.KeyInfo {
	limits := s.api.Limits()
	var out []adminapi.KeyInfo
	for _, k := range s.api.Store().Keys() {
		ki := adminapi.KeyInfo{
			ID: k.ID, Name: k.Name, Agent: k.Agent,
			ConcurrentRequests: k.Limits.ConcurrentRequests,
			RequestsPerSecond:  k.Limits.RequestsPerSecond,
			Burst:              k.Limits.Burst,
			TokensPerHour:      k.Limits.TokensPerHour,
			TokensPerDay:       k.Limits.TokensPerDay,
		}
		if ks, ok := limits.Peek(k.ID); ok {
			ki.Inflight = ks.Concur.InUse()
			ki.ConcurrentRequests = ks.Concur.Limit()
		}
		if k.Limits.TokensPerHour > 0 || k.Limits.TokensPerDay > 0 {
			h, d, hr, dr := s.quota.Usage(k.ID, now)
			ki.HourUsed, ki.DayUsed = h, d
			ki.HourResetsMillis, ki.DayResetsMillis = hr.UnixMilli(), dr.UnixMilli()
		}
		out = append(out, ki)
	}
	return out
}

// servers groups the per-model backends by the server they were
// generated from: admission and metrics belong to the server, health
// to each model's backend on it.
func (s *statsSource) servers() []adminapi.ServerInfo {
	byName := map[string]*adminapi.ServerInfo{}
	var reports map[string]backendmetrics.Reading
	if s.metrics != nil {
		reports = s.metrics()
	}
	modelOf := map[string]string{}
	for model, m := range s.cfg.Models {
		for _, ref := range m.Backends {
			modelOf[ref.Name] = model
		}
	}
	for name, b := range s.cfg.Backends {
		server := cmp.Or(b.Server, name)
		si := byName[server]
		if si == nil {
			si = &adminapi.ServerInfo{Name: server, Kind: "configured", MaxConcurrency: b.MaxConcurrency, QueueSize: b.QueueSize}
			if c := s.clients[name]; c != nil {
				si.Admitted, si.Queued = c.Admitted(), c.Queued()
			}
			if r, ok := reports[server]; ok {
				si.Metrics = &r
			}
			byName[server] = si
		}
		if model := modelOf[name]; model != "" {
			si.Models = append(si.Models, model)
			if until, ok := s.router.CooldownUntil(name); ok {
				si.Health = append(si.Health, adminapi.HealthInfo{Backend: name, Model: model, CooldownUntilMillis: until.UnixMilli()})
			}
		}
	}
	out := make([]adminapi.ServerInfo, 0, len(byName))
	for _, si := range byName {
		slices.Sort(si.Models)
		out = append(out, *si)
	}
	slices.SortFunc(out, func(a, b adminapi.ServerInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// gauges takes this second's live load for the history.
func (s *statsSource) gauges() stats.Gauges {
	g := stats.Gauges{GlobalInflight: s.api.Inflight(), Keys: map[string]stats.KeyGauge{}, Servers: map[string]stats.ServerGauge{}}
	limits := s.api.Limits()
	for _, k := range s.api.Store().Keys() {
		ks, ok := limits.Peek(k.ID)
		if !ok {
			continue
		}
		n, limit := ks.Concur.InUse(), ks.Concur.Limit()
		if n == 0 {
			continue
		}
		g.Keys[k.ID] = stats.KeyGauge{Name: k.Name, Inflight: n, AtLimit: limit > 0 && n >= limit}
	}
	for _, si := range s.servers() {
		sg := stats.ServerGauge{Admitted: si.Admitted, Queued: si.Queued}
		addMetrics(&sg, si.Metrics)
		g.Servers[si.Name] = sg
	}
	if s.agents != nil {
		for _, a := range s.agents.Snapshot() {
			sg := stats.ServerGauge{Admitted: a.Admitted, Queued: a.Queued}
			addMetrics(&sg, a.Metrics)
			g.Servers[a.Name] = sg
		}
	}
	return g
}

func addMetrics(sg *stats.ServerGauge, r *backendmetrics.Reading) {
	if r == nil || !r.HasQueue {
		return
	}
	sg.HasMetrics = true
	sg.Running, sg.Waiting, sg.KVPercent = int(r.Running), int(r.Waiting), r.KVPercent
}

// runSampler records gauges once a second until ctx ends.
func runSampler(ctx context.Context, s *statsSource) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.store.Sample(s.gauges())
		}
	}
}
