package backendmetrics

import (
	"errors"
	"math"
)

// MaxValue caps every figure taken from a report the edge does not
// trust (an agent's), so a lying agent cannot draw absurd graphs.
const MaxValue = 1e12

// Sanitize checks a reading that came from an agent: a known engine,
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
