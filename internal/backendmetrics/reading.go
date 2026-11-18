// Package backendmetrics reads load figures inference servers publish
// about themselves — vLLM and SGLang on /metrics, Ollama on /api/ps —
// so `mellomting top` can show whether a server is running or queueing
// what the edge sent it (PLAN §43.2).
package backendmetrics

import "time"

// Reading is one server's latest report. Engine is "vllm", "sglang" or
// "ollama". HasQueue is false for an engine that reports no queue
// (Ollama), so the view shows its loaded models instead of zeros.
type Reading struct {
	Engine           string        `json:"engine"`
	HasQueue         bool          `json:"has_queue,omitempty"`
	Running          float64       `json:"running,omitempty"`
	Waiting          float64       `json:"waiting,omitempty"`
	KVPercent        float64       `json:"kv_percent,omitempty"`
	PromptTokens     float64       `json:"prompt_tokens_total,omitempty"`
	GenerationTokens float64       `json:"generation_tokens_total,omitempty"`
	Preemptions      float64       `json:"preemptions_total,omitempty"`
	Loaded           []LoadedModel `json:"loaded,omitempty"`
	At               time.Time     `json:"at"`
}

// LoadedModel is a model resident on an Ollama server.
type LoadedModel struct {
	Name      string    `json:"name"`
	VRAMBytes int64     `json:"vram_bytes,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}
