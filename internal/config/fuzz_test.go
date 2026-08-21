package config

import "testing"

// FuzzParse feeds arbitrary bytes to the strict YAML parser. The target:
// no panic, no crash, clean errors only (PLAN §80 fuzz target). Seeds
// include multi-document `---` markers, which must be rejected rather than
// silently dropping a trailing document (T-M7).
func FuzzParse(f *testing.F) {
	f.Add([]byte(validYAML))
	f.Add([]byte{})
	f.Add([]byte("version: 1\n"))
	f.Add([]byte("---\n- a\n- b\n"))
	f.Add([]byte("a: &x 1\nb: *x\n"))
	f.Add([]byte(validYAML + "\n---\nsecurity:\n  landlock:\n    mode: required\n"))
	f.Add([]byte("---\nversion: 1\n---\nversion: 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		Parse(data)
	})
}
