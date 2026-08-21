// Package accounting implements token-usage recording and fixed UTC
// token quotas (PLAN §37-42): a bounded JSONL writer with drop-and-alert
// overflow, backend-reported usage parsing for streaming and non-streaming
// responses, and simple deterministic per-key token windows that can be
// reconstructed by replaying the JSONL log on startup.
//
// Accounting is NOT a tamper-evident audit log (PLAN §41). Prompts,
// responses, tool arguments, and raw backend error bodies are never
// written (PLAN §24, §41, §43).
package accounting

import (
	"encoding/json"
	"time"
)

// UsageStatus classifies how precisely token usage was known (PLAN §38,
// §39).
type UsageStatus string

const (
	// UsageExact means usage came from the backend response.
	UsageExact UsageStatus = "exact"
	// UsagePartial means only a portion of usage was known.
	UsagePartial UsageStatus = "partial"
	// UsageUnknown means no usage was obtained (e.g. a stream ended
	// before usage was emitted); never invent exact counts (PLAN §38).
	UsageUnknown UsageStatus = "unknown"
)

// Record is one JSONL accounting line (PLAN §41). Field names follow the
// documented example exactly so the log is stable and replayable.
type Record struct {
	Time            time.Time   `json:"time"`
	RequestID       string      `json:"request_id"`
	KeyID           string      `json:"key_id"`
	Model           string      `json:"model"`
	Backend         string      `json:"backend"`
	Endpoint        string      `json:"endpoint"`
	Status          int         `json:"status"`
	DurationMS      int64       `json:"duration_ms"`
	InputTokens     int64       `json:"input_tokens"`
	OutputTokens    int64       `json:"output_tokens"`
	TotalTokens     int64       `json:"total_tokens"`
	CachedTokens    int64       `json:"cached_tokens"`
	ReasoningTokens int64       `json:"reasoning_tokens"`
	UsageStatus     UsageStatus `json:"usage_status"`
	Retries         int         `json:"retries"`
	// ChargedTokens is the token count actually settled against quota
	// (PLAN §39): for exact usage it equals total_tokens; for unknown
	// usage it is the configured reservation charged. Replay reconstructs
	// quota from this field so conservatively-charged records survive a
	// restart (PLAN §40).
	ChargedTokens int64 `json:"charged_tokens"`
}

// Usage holds the backend-reported token usage (PLAN §37).
type Usage struct {
	Input     int64
	Output    int64
	Total     int64
	Cached    int64
	Reasoning int64
	Present   bool
}

// usageBody is the OpenAI Chat/Completions usage object shape.
type usageBody struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// responsesUsage is the OpenAI Responses API usage object shape.
type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// ParseUsage parses token usage from a backend response body. endpoint is
// one of "chat.completions", "completions", "responses", or "" (best
// effort). Both the Chat/Completions and the Responses usage shapes are
// recognized. Present is false when the body carries no usage.
func ParseUsage(body []byte, endpoint string) Usage {
	if len(body) == 0 {
		return Usage{}
	}
	var top struct {
		Usage    json.RawMessage `json:"usage"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return Usage{}
	}
	var u Usage
	if len(top.Usage) > 0 {
		if u = fromUsageJSON(top.Usage); u.Present {
			return u
		}
	}
	if len(top.Response.Usage) > 0 {
		if u = fromUsageJSON(top.Response.Usage); u.Present {
			return u
		}
	}
	return Usage{}
}

// maxUsageTokens bounds a single backend-reported usage value. It is far
// beyond any real model's token count, so a bogus extreme value cannot
// wrap quota counters (PLAN §39).
const maxUsageTokens int64 = 1 << 40

// clampUsage bounds a single usage value to [0, maxUsageTokens] so a
// malformed or malicious backend report cannot inject negatives or values
// that overflow the quota windows (PLAN §39).
func clampUsage(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxUsageTokens {
		return maxUsageTokens
	}
	return v
}

func fromUsageJSON(raw json.RawMessage) Usage {
	// Decide the shape by the presence of shape-specific field names
	// rather than by values, so a chat body is never misread as a
	// responses body (which would zero out prompt/completion tokens).
	var keyCheck struct {
		InputTokens  json.RawMessage `json:"input_tokens"`
		PromptTokens json.RawMessage `json:"prompt_tokens"`
	}
	_ = json.Unmarshal(raw, &keyCheck)
	if len(keyCheck.PromptTokens) > 0 {
		// Chat / Completions shape.
		var ub usageBody
		if err := json.Unmarshal(raw, &ub); err != nil {
			return Usage{}
		}
		if ub.PromptTokens == 0 && ub.CompletionTokens == 0 && ub.TotalTokens == 0 {
			return Usage{}
		}
		u := Usage{
			Input:   clampUsage(ub.PromptTokens),
			Output:  clampUsage(ub.CompletionTokens),
			Total:   clampUsage(ub.TotalTokens),
			Present: true,
		}
		if ub.PromptDetails != nil {
			u.Cached = clampUsage(ub.PromptDetails.CachedTokens)
		}
		if ub.CompletionDetails != nil {
			u.Reasoning = clampUsage(ub.CompletionDetails.ReasoningTokens)
		}
		if u.Total == 0 {
			u.Total = satAdd(u.Input, u.Output)
		}
		return u
	}
	if len(keyCheck.InputTokens) > 0 {
		// Responses shape.
		var ru responsesUsage
		if err := json.Unmarshal(raw, &ru); err != nil {
			return Usage{}
		}
		if ru.InputTokens == 0 && ru.OutputTokens == 0 && ru.TotalTokens == 0 {
			return Usage{}
		}
		u := Usage{
			Input:   clampUsage(ru.InputTokens),
			Output:  clampUsage(ru.OutputTokens),
			Total:   clampUsage(ru.TotalTokens),
			Present: true,
		}
		if ru.InputDetails != nil {
			u.Cached = clampUsage(ru.InputDetails.CachedTokens)
		}
		if ru.OutputDetails != nil {
			u.Reasoning = clampUsage(ru.OutputDetails.ReasoningTokens)
		}
		if u.Total == 0 {
			u.Total = satAdd(u.Input, u.Output)
		}
		return u
	}
	return Usage{}
}

// ParseStreamChunk parses a single SSE data payload for token usage
// (PLAN §38). It recognizes a top-level "usage" object (Chat/Completions
// final chunk) and the Responses API's nested response.usage.
func ParseStreamChunk(data string) Usage {
	return ParseUsage([]byte(data), "")
}

// MarshalJSON serializes the record with the canonical field set.
func (r Record) MarshalJSON() ([]byte, error) {
	type alias Record
	return json.Marshal(alias(r))
}

// UnmarshalRecord parses one JSONL line into a Record. It tolerates the
// "cached_tokens" field being absent and defaults UsageStatus to unknown
// when missing.
func UnmarshalRecord(line []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return Record{}, err
	}
	if r.UsageStatus == "" {
		r.UsageStatus = UsageUnknown
	}
	return r, nil
}

// SettledTokens returns the total tokens that count toward quota for this
// record. New records carry the actual charged amount; legacy records fall
// back to total_tokens (PLAN §39).
func (r Record) SettledTokens() int64 {
	if r.ChargedTokens > 0 {
		return r.ChargedTokens
	}
	return r.TotalTokens
}

// hourKey is the start of the fixed UTC hour window for a time.
func hourKey(t time.Time) int64 {
	return t.UTC().Unix() - t.UTC().Unix()%3600
}

// dayKey is the start of the fixed UTC day window for a time.
func dayKey(t time.Time) int64 {
	return t.UTC().Unix() - t.UTC().Unix()%86400
}
