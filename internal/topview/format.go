package topview

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// num prints a count or rate compactly: 0.4, 42, 3.1k, 12M.
func num(v float64) string {
	switch a := math.Abs(v); {
	case v == 0:
		return "0"
	case a < 10:
		return trimZero(fmt.Sprintf("%.1f", v))
	case a < 1000:
		return fmt.Sprintf("%.0f", v)
	case a < 1e6:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case a < 1e9:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + "M"
	}
	return trimZero(fmt.Sprintf("%.1f", v/1e9)) + "G"
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// size prints bytes: 512 B, 12 KB, 3.4 MB.
func size(v float64) string {
	switch {
	case v < 1000:
		return fmt.Sprintf("%.0f B", v)
	case v < 1e6:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + " KB"
	case v < 1e9:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + " MB"
	}
	return trimZero(fmt.Sprintf("%.1f", v/1e9)) + " GB"
}

// dur prints a duration in its two largest units: 850ms, 41s, 2m31s,
// 17h09m.
func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return trimZero(fmt.Sprintf("%.1fs", d.Seconds()))
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// limitName is the configuration key behind a refusal reason, as an
// operator would look it up.
func limitName(reason string) string {
	switch reason {
	case "key_concurrency":
		return "concurrent_requests"
	case "key_rate":
		return "requests_per_second"
	case "global_inflight":
		return "max_inflight_requests"
	case "global_rate":
		return "global rate limit"
	case "source_rate":
		return "per-source rate limit"
	case "quota":
		return "token quota"
	case "backend_queue":
		return "backend queue"
	case "auth":
		return "authentication"
	}
	return reason
}

// engineName is how a backend engine is written.
func engineName(e string) string {
	switch e {
	case "vllm":
		return "vLLM"
	case "sglang":
		return "SGLang"
	case "ollama":
		return "Ollama"
	}
	return e
}
