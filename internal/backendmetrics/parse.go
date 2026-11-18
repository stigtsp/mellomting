package backendmetrics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxBody bounds a /metrics page. vLLM's runs to tens of KiB; the
	// bound leaves room for histograms on many models.
	MaxBody = 1 << 20
	// maxLine skips label-heavy lines rather than buffering them; the
	// allow-listed series are short.
	maxLine = 4 << 10
	// MaxOllamaBody bounds an /api/ps reply.
	MaxOllamaBody = 256 << 10
	// MaxLoaded bounds the loaded models kept from one Ollama server.
	MaxLoaded = 16
	// maxModelName bounds a loaded model's name as displayed.
	maxModelName = 128
)

var errTooLarge = errors.New("metrics page too large")

// gauge kinds the allow-list maps names to.
type kind int

const (
	running kind = iota
	waiting
	cachePrimary  // vLLM's current name; wins when present
	cacheFallback // vLLM's older name
	prompt
	generation
	preemptions
)

type metric struct {
	engine string
	kind   kind
}

// allow is every series kept. Anything else is skipped before its
// labels are looked at.
var allow = map[string]metric{
	"vllm:num_requests_running":      {"vllm", running},
	"vllm:num_requests_waiting":      {"vllm", waiting},
	"vllm:kv_cache_usage_perc":       {"vllm", cachePrimary},
	"vllm:gpu_cache_usage_perc":      {"vllm", cacheFallback},
	"vllm:prompt_tokens_total":       {"vllm", prompt},
	"vllm:generation_tokens_total":   {"vllm", generation},
	"vllm:num_preemptions_total":     {"vllm", preemptions},
	"sglang:num_running_reqs":        {"sglang", running},
	"sglang:num_queue_reqs":          {"sglang", waiting},
	"sglang:token_usage":             {"sglang", cachePrimary},
	"sglang:prompt_tokens_total":     {"sglang", prompt},
	"sglang:generation_tokens_total": {"sglang", generation},
}

// Parse reads a Prometheus text page from vLLM or SGLang. ok is false
// when the page has no running or waiting series: another Mellomting,
// a non-inference server, or an engine started without metrics.
// Values that are not finite and non-negative are dropped.
func Parse(r io.Reader) (Reading, bool, error) {
	var out Reading
	br := bufio.NewReaderSize(r, maxLine)
	var total int
	var primary, fallback float64
	var havePrimary, haveFallback bool
	for {
		line, err := br.ReadSlice('\n')
		total += len(line)
		if total > MaxBody {
			return Reading{}, false, errTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Over maxLine: discard the rest of it.
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = br.ReadSlice('\n')
				total += len(line)
				if total > MaxBody {
					return Reading{}, false, errTooLarge
				}
			}
			if err != nil && err != io.EOF {
				return Reading{}, false, err
			}
			if err == io.EOF {
				break
			}
			continue
		}
		if len(line) > 0 {
			if m, v, ok := sample(line); ok {
				if out.Engine == "" {
					out.Engine = m.engine
				}
				switch m.kind {
				case running:
					out.Running += v
					out.HasQueue = true
				case waiting:
					out.Waiting += v
					out.HasQueue = true
				case cachePrimary:
					primary, havePrimary = max(primary, v), true
				case cacheFallback:
					fallback, haveFallback = max(fallback, v), true
				case prompt:
					out.PromptTokens += v
				case generation:
					out.GenerationTokens += v
				case preemptions:
					out.Preemptions += v
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Reading{}, false, err
		}
	}
	if !out.HasQueue {
		return Reading{}, false, nil
	}
	switch {
	case havePrimary:
		out.KVPercent = min(primary*100, 100)
	case haveFallback:
		out.KVPercent = min(fallback*100, 100)
	}
	out.At = time.Now()
	return out, true, nil
}

// sample parses one allow-listed series line: name, optional labels,
// value, optional timestamp.
func sample(line []byte) (metric, float64, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] == '#' {
		return metric{}, 0, false
	}
	end := bytes.IndexAny(line, "{ \t")
	if end < 0 {
		return metric{}, 0, false
	}
	m, ok := allow[string(line[:end])]
	if !ok {
		return metric{}, 0, false
	}
	rest := line[end:]
	if rest[0] == '{' {
		close := bytes.LastIndexByte(rest, '}')
		if close < 0 {
			return metric{}, 0, false
		}
		rest = rest[close+1:]
	}
	fields := bytes.Fields(rest)
	if len(fields) == 0 {
		return metric{}, 0, false
	}
	v, err := strconv.ParseFloat(string(fields[0]), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return metric{}, 0, false
	}
	return m, v, true
}

// ParseOllamaPS reads Ollama's GET /api/ps: the models loaded now. It
// reports no queue: Ollama publishes none.
func ParseOllamaPS(r io.Reader) (Reading, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxOllamaBody+1))
	if err != nil {
		return Reading{}, err
	}
	if len(body) > MaxOllamaBody {
		return Reading{}, errTooLarge
	}
	var ps struct {
		Models []struct {
			Name      string    `json:"name"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &ps); err != nil {
		return Reading{}, err
	}
	out := Reading{Engine: "ollama", At: time.Now()}
	for _, m := range ps.Models {
		if len(out.Loaded) == MaxLoaded {
			break
		}
		out.Loaded = append(out.Loaded, LoadedModel{Name: clean(m.Name), VRAMBytes: max(m.SizeVRAM, 0), ExpiresAt: m.ExpiresAt})
	}
	return out, nil
}

// clean drops control characters and bounds the length: a model name
// is the remote server's text and ends up on an operator's terminal.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxModelName {
		s = s[:maxModelName]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
