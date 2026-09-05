package auth

import (
	"testing"
)

// BenchmarkAuthCheck measures the full per-request bearer-key check:
// HMAC-SHA-256 derivation plus the constant-time comparison (PLAN §25,
// §87). It deliberately never uses a password KDF.
func BenchmarkAuthCheck(b *testing.B) {
	pepper := []byte("benchmark-pepper-16b")
	rawKey, id, err := Generate("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	uf := &UsersFile{Version: 1, Keys: []Key{{
		ID:         id,
		Name:       "benchmark",
		SecretHash: FormatHashValue(Hash(pepper, rawKey)),
		Enabled:    true,
		Models:     []string{"*"},
	}}}
	store, err := NewStore(uf, pepper)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Lookup(rawKey); err != nil {
			b.Fatal(err)
		}
	}
}
