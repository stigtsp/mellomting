package backendmetrics

import (
	"errors"
	"math"
)

// MaxValue caps every figure taken from a server's or an agent's
// report, so neither can draw absurd graphs or produce an +Inf that
// /stats cannot encode.
const MaxValue = 1e12

// Sanitize checks a reading, from a server or an agent: a known engine,
// every figure finite and within [0, MaxValue], cache use at most
// 100 %, at most MaxLoaded models with clean names.
func Sanitize(r *Reading) error {
	switch r.Engine {
	case "vllm", "sglang", "ollama":
	default:
		return errors.New("unknown engine")
	}
	for _, v := range []*float64{&r.Running, &r.Waiting, &r.KVPercent, &r.PromptTokens, &r.GenerationTokens, &r.Preemptions} {
		switch {
		case math.IsNaN(*v) || *v < 0:
			*v = 0
		case *v > MaxValue:
			*v = MaxValue
		}
	}
	r.KVPercent = min(r.KVPercent, 100)
	if len(r.Loaded) > MaxLoaded {
		r.Loaded = r.Loaded[:MaxLoaded]
	}
	for i := range r.Loaded {
		r.Loaded[i].Name = clean(r.Loaded[i].Name)
		r.Loaded[i].VRAMBytes = max(r.Loaded[i].VRAMBytes, 0)
	}
	return nil
}
