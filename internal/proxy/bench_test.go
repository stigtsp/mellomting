package proxy

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

var benchChatBody = []byte(`{"model":"qwen-coder","messages":[{"role":"user","content":"summarize the meeting"}],"stream":true,"max_tokens":512,"temperature":0.7}`)

// BenchmarkShallowParseAndRewrite measures the per-request request JSON
// rewrite: the shallow parse plus the model rewrite before forwarding
// (PLAN §87).
func BenchmarkShallowParseAndRewrite(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		body, _, _, err := shallowParse(benchChatBody)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := rewriteModel(body, "Up/qwen-coder"); err != nil {
			b.Fatal(err)
		}
	}
}

var benchSSEStream = func() []byte {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token stream \"}}]}\n\n")
	}
	return []byte(sb.String())
}()

// BenchmarkSSEParse measures the SSE parse step of streaming a response
// (PLAN §87): event boundary detection and framing for a 100-event
// stream.
func BenchmarkSSEParse(b *testing.B) {
	src := benchSSEStream
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := newSSEParser(bytes.NewReader(src))
		for {
			_, err := p.nextEvent()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkAffinityPutEmpty is the baseline: Put into an affinity table
// that never evicts (FIX-16 comparison point).
func BenchmarkAffinityPutEmpty(b *testing.B) {
	a := newAffinity(time.Hour, 1<<30)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("resp_%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Put("K", keys[i%len(keys)], "b1", "gen-1")
	}
}

// BenchmarkAffinityPutFull measures Put on a full affinity table at the
// MaxAffinityEntries bound (FIX-16): eviction must be bounded work rather
// than two full-table scans per Put (the review measured a 1712× cliff
// against the empty case).
func BenchmarkAffinityPutFull(b *testing.B) {
	a := newAffinity(time.Hour, 10000)
	keys := make([]string, 10000)
	for i := range keys {
		keys[i] = fmt.Sprintf("resp_%d", i)
	}
	for i := 0; i < 10000; i++ {
		a.Put("K", keys[i], "b1", "gen-1")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Put("K", keys[i%len(keys)], "b1", "gen-1")
	}
}
