package auth

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mellomting/internal/config"
)

// FuzzParseKey feeds arbitrary bytes to the API-key parser (PLAN §80
// "API-key parser"). It must never panic: every malformed input returns a
// clean error and never reveals which part was malformed.
func FuzzParseKey(f *testing.F) {
	f.Add("sk-a-0000000000000000-000000000000000000000000000000000000000000000000000000")
	f.Add("")
	f.Add("sk")
	f.Add("sk-1-2-3")
	f.Add("sk-a-0000000000000000-000000000000000000000000000000000000000000000000000000")
	f.Add("notakey")
	f.Add(strings.Repeat("sk-a-b-", 100))
	f.Fuzz(func(t *testing.T, raw string) {
		Parse(raw)
	})
}

// FuzzParseHashValue feeds arbitrary bytes to the stored-hash decoder
// (PLAN §80, part of the API-key parser family). It must never panic and
// only ever report a supported/malformed encoding error.
func FuzzParseHashValue(f *testing.F) {
	f.Add("hmac-sha256:" + strings.Repeat("A", 44))
	f.Add("")
	f.Add("hmac-sha256:")
	f.Add("sha256:abc")
	f.Add(strings.Repeat("hmac-sha256:AA==", 100))
	f.Fuzz(func(t *testing.T, stored string) {
		ParseHashValue(stored)
	})
}

// FuzzStoreLookup feeds arbitrary bytes to the key-store lookup (PLAN
// §80 "API-key parser" family). The store is built once; every lookup
// must classify (found/disabled/unknown) without panicking, and an
// unknown key must never surface the raw key in an error.
func FuzzStoreLookup(f *testing.F) {
	pepper := []byte("fuzz-pepper-16bytes")
	key, id, err := Generate("f")
	if err != nil {
		f.Fatal(err)
	}
	uf := &UsersFile{Version: 1, Keys: []Key{
		{ID: id, Name: "f", SecretHash: FormatHashValue(Hash(pepper, key)), Enabled: true, Models: []string{"*"}},
	}}
	store, err := NewStore(uf, pepper)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(key)
	f.Add("")
	f.Add("sk-f-0000000000000000-000000000000000000000000000000000000000000000000000000")
	f.Add(strings.Repeat("x", 512))
	f.Fuzz(func(t *testing.T, raw string) {
		_, err := store.Lookup(raw)
		if err == nil {
			return
		}
		// Lookup must only ever fail with a fixed sentinel error (PLAN
		// §27: never embed the raw key in an error). Any other error is
		// a leak, since an error embedding the raw key cannot be a
		// fixed sentinel.
		if !errors.Is(err, ErrUnknownKey) && !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrExpired) {
			t.Fatalf("lookup returned a non-sentinel error: %v", err)
		}
	})
}

// FuzzUsersParser feeds arbitrary bytes to the YAML users-file parser
// (PLAN §80 "YAML users parser"): the same alias/anchor/merge rejection,
// strict KnownFields decode, and validation LoadUsers applies. It must
// never panic; malformed files return a clean error.
func FuzzUsersParser(f *testing.F) {
	f.Add([]byte("version: 1\nkeys:\n  - id: 0000000000000000\n    name: a\n    secret_hash: hmac-sha256:AA==\n    models: ['*']\n"))
	f.Add([]byte("---\n- a\n- b\n"))
	f.Add([]byte("version: 1\n"))
	f.Add([]byte("a: &x 1\nb: *x\n"))
	f.Add([]byte("version: 1\nkeys:\n  - id: 0000000000000000\n    name: a\n    secret_hash: hmac-sha256:AA==\n    models: ['*']\n---\nversion: 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Mirror LoadUsers (store.go): alias/anchor/merge rejection,
		// strict KnownFields decode, then validation.
		if err := config.CheckYAMLTree(data); err != nil {
			return
		}
		var uf UsersFile
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&uf); err != nil {
			return
		}
		_ = validateUsers(&uf)
	})
}
