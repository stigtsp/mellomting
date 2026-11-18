package backendmetrics

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseVLLM(t *testing.T) {
	r, ok, err := Parse(open(t, "vllm.txt"))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Engine != "vllm" || !r.HasQueue || r.Running != 6 || r.Waiting != 18 {
		t.Fatalf("reading %+v", r)
	}
	if r.KVPercent != 94 {
		t.Fatalf("kv %v: kv_cache_usage_perc must win over gpu_cache_usage_perc", r.KVPercent)
	}
	if r.PromptTokens != 1234500 || r.GenerationTokens != 54321 || r.Preemptions != 3 {
		t.Fatalf("counters %+v", r)
	}
}

func TestParseVLLMOldCacheName(t *testing.T) {
	r, _, _ := Parse(strings.NewReader("vllm:num_requests_running 1\nvllm:gpu_cache_usage_perc{a=\"b\"} 0.25\n"))
	if r.KVPercent != 25 {
		t.Fatalf("kv %v", r.KVPercent)
	}
}

func TestParseSGLang(t *testing.T) {
	r, ok, err := Parse(open(t, "sglang.txt"))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Engine != "sglang" || !r.HasQueue || r.Running != 6 || r.Waiting != 18 || r.KVPercent != 50 ||
		r.PromptTokens != 8.6e6 || r.GenerationTokens != 1.1e6 {
		t.Fatalf("reading %+v", r)
	}
}

func TestParseIgnoresOthers(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"not found","type":"invalid_request_error"}}`, // another Mellomting
		"go_goroutines 12\nprocess_cpu_seconds_total 3\n",
		"",
	} {
		if _, ok, err := Parse(strings.NewReader(body)); ok || err != nil {
			t.Errorf("%q: ok=%v err=%v", body, ok, err)
		}
	}
}

func TestParseBounds(t *testing.T) {
	big := strings.Repeat("# padding padding padding padding padding\n", MaxBody/40+10)
	if _, _, err := Parse(strings.NewReader(big)); err == nil {
		t.Fatal("body over the bound accepted")
	}
	long := "vllm:num_requests_running{x=\"" + strings.Repeat("a", 5000) + "\"} 99\nvllm:num_requests_waiting 2\n"
	r, ok, err := Parse(strings.NewReader(long))
	if err != nil || !ok || r.Running != 0 || r.Waiting != 2 {
		t.Fatalf("long line: %+v ok=%v err=%v", r, ok, err)
	}
	for _, v := range []string{"NaN", "+Inf", "-Inf", "-3", "1e400"} {
		r, _, _ := Parse(strings.NewReader(fmt.Sprintf("vllm:num_requests_waiting 1\nvllm:num_requests_running %s\n", v)))
		if r.Running != 0 {
			t.Errorf("%s accepted as %v", v, r.Running)
		}
	}
}

func TestParseOllamaPS(t *testing.T) {
	r, err := ParseOllamaPS(open(t, "ollama_ps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Engine != "ollama" || r.HasQueue || len(r.Loaded) != 2 {
		t.Fatalf("reading %+v", r)
	}
	m := r.Loaded[0]
	if m.Name != "local-30b" || m.VRAMBytes != 19e9 || !m.ExpiresAt.Equal(time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("model %+v", m)
	}
}

func TestParseOllamaPSBounds(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"models":[`)
	for i := range 17 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"m%d","size_vram":1}`, i)
	}
	b.WriteString("]}")
	r, err := ParseOllamaPS(strings.NewReader(b.String()))
	if err != nil || len(r.Loaded) != MaxLoaded {
		t.Fatalf("loaded %d err %v", len(r.Loaded), err)
	}
	if _, err := ParseOllamaPS(strings.NewReader(`{"models":[{"name":"` + strings.Repeat("x", MaxOllamaBody) + `"}]}`)); err == nil {
		t.Fatal("oversized /api/ps accepted")
	}
	if _, err := ParseOllamaPS(strings.NewReader(`not json`)); err == nil {
		t.Fatal("garbage accepted")
	}
	r, _ = ParseOllamaPS(strings.NewReader(`{"models":[{"name":"\u001b[31mevil","size_vram":-5}]}`))
	if strings.ContainsRune(r.Loaded[0].Name, 0x1b) || r.Loaded[0].VRAMBytes != 0 {
		t.Fatalf("unsanitized %+v", r.Loaded[0])
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("vllm:num_requests_running{a=\"b\"} 3\n"))
	f.Add([]byte("sglang:token_usage 0.5\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, _, _ := Parse(strings.NewReader(string(b)))
		if r.Running < 0 || r.Waiting < 0 || r.KVPercent < 0 || r.KVPercent > 100 {
			t.Fatalf("out of range %+v", r)
		}
	})
}

func FuzzParseOllamaPS(f *testing.F) {
	f.Add([]byte(`{"models":[{"name":"a","size_vram":1}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, _ := ParseOllamaPS(strings.NewReader(string(b)))
		if len(r.Loaded) > MaxLoaded {
			t.Fatal("unbounded")
		}
	})
}
