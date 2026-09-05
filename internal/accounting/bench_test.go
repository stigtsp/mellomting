package accounting

import (
	"path/filepath"
	"testing"
	"time"
)

var benchUsageBody = []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1200,"completion_tokens":340,"total_tokens":1540,"prompt_tokens_details":{"cached_tokens":900},"completion_tokens_details":{"reasoning_tokens":60}}}`)

// BenchmarkParseUsage measures usage extraction from a backend response
// (PLAN §87): the tolerated-shallow usage parse per completed request.
func BenchmarkParseUsage(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = ParseUsage(benchUsageBody, "/v1/chat/completions")
	}
}

// BenchmarkEnqueue measures the accounting enqueue path for a completed
// request (PLAN §87): the bounded, non-blocking channel submit.
func BenchmarkEnqueue(b *testing.B) {
	w, err := NewWriter(WriterConfig{
		Path:      filepath.Join(b.TempDir(), "accounting.jsonl"),
		QueueSize: 1 << 20,
		FSync:     "never",
		Overflow:  "drop-and-alert",
	})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	rec := Record{
		Time:         time.Now(),
		RequestID:    "req_bench",
		KeyID:        "key_bench",
		Model:        "qwen-coder",
		Backend:      "back-a",
		Endpoint:     "/v1/chat/completions",
		Status:       200,
		InputTokens:  1200,
		OutputTokens: 340,
		TotalTokens:  1540,
		CachedTokens: 900,
	}
	b.ReportAllocs()
	for b.Loop() {
		w.Enqueue(rec)
	}
}
