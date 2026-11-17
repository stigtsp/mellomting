package agent

import "testing"

func TestValidAgentModelID(t *testing.T) {
	for id, want := range map[string]bool{
		"qwen3":                      true,
		"Qwen/Qwen2.5-7B-Instruct":   true,
		"meta/llama-3.1-8b-instruct": true,
		"smollm2:135m":               true,
		"model@v1+fp8_x":             true,
		"":                           false,
		"has space":                  false,
		"gpt‑4o":                     false, // non-breaking hyphen lookalike
		"café":                       false,
		"bidi\u202ename":             false,
		"semi;colon":                 false,
	} {
		if got := validAgentModelID(id); got != want {
			t.Errorf("validAgentModelID(%q) = %v, want %v", id, got, want)
		}
	}
}
