package accounting

import "testing"

// FuzzParseUsage feeds arbitrary bytes to the usage parser (PLAN §80
// "streaming usage extraction" target). The invariants: no panic, and any
// reported token value is clamped into [0, maxUsageTokens], so an extreme
// bogus value can never wrap a quota counter (T-X11).
func FuzzParseUsage(f *testing.F) {
	f.Add([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	f.Add([]byte(`{"response":{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`))
	f.Add([]byte(`{"usage":{"total_tokens":9223372036854775807}}`))
	f.Add([]byte(`{"usage":{"prompt_tokens":-1,"total_tokens":-5}}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		u := ParseUsage(data, "")
		if !u.Present {
			return
		}
		for _, v := range []int64{u.Input, u.Output, u.Total, u.Cached, u.Reasoning} {
			if v < 0 || v > maxUsageTokens {
				t.Fatalf("unclamped usage value: %d", v)
			}
		}
	})
}
