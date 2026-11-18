package adminapi

import (
	"mellomting/internal/agent"
	"mellomting/internal/backendmetrics"
	"mellomting/internal/inflight"
	"mellomting/internal/stats"
)

// MaxStatsBytes bounds a /stats reply; top reads at most 4 MiB.
const MaxStatsBytes = 3 << 20

// Stats is the response of GET /stats: the throughput history and
// everything `mellomting top` sets beside it — limits, live load,
// backend reports — so one poll draws a whole frame.
type Stats struct {
	NowMillis     int64        `json:"now_ms"`
	StartedMillis int64        `json:"started_ms"`
	Global        GlobalInfo   `json:"global"`
	Stats         stats.Export `json:"stats"`
	// Keys lists every key in the store, so history from usage.jsonl
	// resolves to names; secrets are never included.
	Keys     []KeyInfo         `json:"keys"`
	Servers  []ServerInfo      `json:"servers"`
	Agents   []agent.Record    `json:"agents"`
	Requests []inflight.Record `json:"requests"`
}

// GlobalInfo is the daemon-wide limits and their live use.
type GlobalInfo struct {
	MaxInflight int            `json:"max_inflight"`
	Inflight    int            `json:"inflight"`
	RPS         float64        `json:"rps,omitempty"`
	Burst       int            `json:"burst,omitempty"`
	Accounting  AccountingInfo `json:"accounting"`
}

// AccountingInfo says whether usage is recorded, and how many records
// the writer dropped because the disk could not keep up.
type AccountingInfo struct {
	Enabled bool  `json:"enabled"`
	Dropped int64 `json:"dropped,omitempty"`
}

// KeyInfo is a key's limits beside its live use.
type KeyInfo struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Agent              string  `json:"agent,omitempty"`
	ConcurrentRequests int     `json:"concurrent_requests,omitempty"`
	RequestsPerSecond  float64 `json:"requests_per_second,omitempty"`
	Burst              int     `json:"burst,omitempty"`
	Inflight           int     `json:"inflight,omitempty"`
	TokensPerHour      int64   `json:"tokens_per_hour,omitempty"`
	TokensPerDay       int64   `json:"tokens_per_day,omitempty"`
	HourUsed           int64   `json:"hour_used,omitempty"`
	DayUsed            int64   `json:"day_used,omitempty"`
	HourResetsMillis   int64   `json:"hour_resets_ms,omitempty"`
	DayResetsMillis    int64   `json:"day_resets_ms,omitempty"`
}

// ServerInfo is one inference server (Kind "configured") or agent
// attachment (Kind "agent"): the edge's admission beside the server's
// own report.
type ServerInfo struct {
	Name           string                  `json:"name"`
	Kind           string                  `json:"kind"`
	Models         []string                `json:"models,omitempty"`
	Admitted       int                     `json:"admitted"`
	Queued         int                     `json:"queued"`
	MaxConcurrency int                     `json:"max_concurrency"`
	QueueSize      int                     `json:"queue_size"`
	Health         []HealthInfo            `json:"health,omitempty"`
	Metrics        *backendmetrics.Reading `json:"metrics,omitempty"`
}

// HealthInfo is a model's backend on this server in passive-health
// cooldown.
type HealthInfo struct {
	Backend             string `json:"backend"`
	Model               string `json:"model"`
	CooldownUntilMillis int64  `json:"cooldown_until_ms"`
}

// StatsSource assembles a Stats reply. The daemon implements it; the
// handler only parses the request and encodes the result.
type StatsSource interface {
	Stats(since int64) Stats
}
