package auth

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestStore builds a store holding one key for the given raw key.
func newTestStore(t *testing.T, rawKey string) *Store {
	t.Helper()
	pepper := []byte("test-pepper-material")
	parsed, err := Parse(rawKey)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &Key{
		ID:         parsed.ID,
		Name:       "test",
		SecretHash: FormatHashValue(Hash(pepper, rawKey)),
		Enabled:    true,
		Models:     []string{"model-a", "*"},
	}
	return &Store{
		byID:      map[string]*Key{rec.ID: rec},
		pepper:    pepper,
		dummyHash: make([]byte, 32),
		now:       time.Now,
	}
}

func TestGenerateAndParseRoundTrip(t *testing.T) {
	t.Parallel()

	for range 32 {
		key, id, err := Generate("test")
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		parsed, err := Parse(key)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if parsed.ID != id {
			t.Fatalf("ID = %q, want %q", parsed.ID, id)
		}
	}
}

func TestParseRejectsMalformedKeys(t *testing.T) {
	id16 := strings.Repeat("0", 16)
	secret64 := strings.Repeat("0", 64)
	cases := []string{
		"",
		"sk",
		"sk-",
		"sk-abc",
		"sk-abc-" + id16,
		"sk-abc-" + id16 + "-" + strings.Repeat("0", 63),              // short secret
		"sk-abc-" + id16 + "-" + secret64 + "-extra",                  // too many segments
		"sk-ABC-" + id16 + "-" + secret64,                             // uppercase username
		"sk-1abc-" + id16 + "-" + secret64,                            // username starts with digit
		"sk--" + id16 + "-" + secret64,                                // empty username
		"sk-abc-" + strings.Repeat("0", 15) + "-" + secret64,          // 15-char id
		"sk-abc-" + strings.Repeat("0", 17) + "-" + secret64,          // 17-char id
		"sk-abc-" + strings.Repeat("0", 15) + "g-" + secret64,         // non-hex id
		"sk-abc-" + strings.Repeat("0", 15) + "A-" + secret64,         // uppercase id
		"sk-abc-" + id16 + "-" + strings.Repeat("0", 63) + "A",        // uppercase secret
		"sk-abc-" + id16 + "-" + strings.Repeat("0", 31) + "!",        // bad secret char
		"sk-" + strings.Repeat("a", 33) + "-" + id16 + "-" + secret64, // username 33 chars
		"sk-abc_def-" + id16 + "-" + secret64,                         // underscore in username
		"sk-abc-def-" + id16 + "-" + secret64,                         // hyphen splits username
		"mtk_7R3F2V_secretsecretsecretsecretsecret",                   // legacy format
	}
	for _, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded", raw)
		}
	}
	// A well-formed key parses and recovers the fixed-format fields.
	good := "sk-abc-" + id16 + "-" + secret64
	parsed, err := Parse(good)
	if err != nil {
		t.Fatalf("Parse(%q): %v", good, err)
	}
	if parsed.Username != "abc" || parsed.ID != id16 || parsed.Raw != good {
		t.Fatalf("parsed = %+v, want username=abc id=%s", parsed, id16)
	}

	// Username boundary: the maximum 32-character name parses; 1 and 32 are
	// both legal.
	for _, user := range []string{"a", strings.Repeat("a", 32)} {
		raw := "sk-" + user + "-" + id16 + "-" + secret64
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if p.Username != user {
			t.Fatalf("username = %q, want %q", p.Username, user)
		}
	}
}

// D18: raw key input is bounded to MaxRawKeyBytes; an oversized input is
// rejected before any segment is parsed.
func TestParseRejectsOversized(t *testing.T) {
	// A valid 32-char username plus 16/64 hex = 117 bytes is the maximum.
	maxKey := "sk-" + strings.Repeat("a", 32) + "-" + strings.Repeat("0", 16) + "-" + strings.Repeat("0", 64)
	if len(maxKey) != MaxRawKeyBytes {
		t.Fatalf("max key length = %d, want %d", len(maxKey), MaxRawKeyBytes)
	}
	if _, err := Parse(maxKey); err != nil {
		t.Fatalf("Parse(max): %v", err)
	}
	// One byte over the bound is rejected.
	if _, err := Parse(maxKey + "0"); err == nil {
		t.Fatal("oversized key accepted")
	}
}

func TestLookupAcceptsValidKey(t *testing.T) {
	key, _, err := Generate("test")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, key)
	got, err := s.Lookup(key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "test" {
		t.Fatalf("record = %+v", got)
	}
	if !got.Allows("model-a") || !got.Allows("anything") {
		t.Fatal("ACL wrong")
	}
}

func TestLookupUnknownKey(t *testing.T) {
	s := newTestStore(t, mustKey(t))
	if _, err := s.Lookup("sk-test-" + strings.Repeat("b", 16) + "-" + strings.Repeat("b", 64)); err != ErrUnknownKey {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	// Malformed input must also read as unknown, not a format complaint.
	if _, err := s.Lookup("garbage"); err != ErrUnknownKey {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestLookupDisabledAndExpired(t *testing.T) {
	id := strings.Repeat("a", 16)
	key := "sk-test-" + id + "-" + strings.Repeat("a", 64)
	s := newTestStore(t, key)
	rec := s.byID[id]

	if _, err := s.Lookup(key); err != nil {
		t.Fatalf("valid lookup: %v", err)
	}

	rec.Enabled = false
	if _, err := s.Lookup(key); err != ErrDisabled {
		t.Fatalf("disabled: err = %v", err)
	}

	rec.Enabled = true
	exp := time.Now().Add(-time.Hour)
	rec.ExpiresAt = &exp
	if _, err := s.Lookup(key); err != ErrExpired {
		t.Fatalf("expired: err = %v", err)
	}

	exp = time.Now().Add(time.Hour)
	rec.ExpiresAt = &exp
	if _, err := s.Lookup(key); err != nil {
		t.Fatalf("future expiry: %v", err)
	}
}

func TestUsersFileRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.yaml")
	pepper := []byte("round-trip-pepper")

	var createdKey, createdID string
	err := Update(usersPath, func(uf *UsersFile) error {
		key, id, err := Generate("codex")
		if err != nil {
			return err
		}
		createdKey, createdID = key, id
		uf.Keys = append(uf.Keys, Key{
			ID:         id,
			Name:       "codex",
			SecretHash: FormatHashValue(Hash(pepper, key)),
			Enabled:    true,
			Models:     []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	uf, err := LoadUsers(usersPath)
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	if len(uf.Keys) != 1 || uf.Keys[0].ID != createdID {
		t.Fatalf("users file wrong: %+v", uf.Keys)
	}

	store, err := NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(createdKey); err != nil {
		t.Fatalf("Lookup after round trip: %v", err)
	}

	mode, err := os.Stat(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := mode.Mode().Perm(); perm != 0o600 {
		t.Fatalf("users file mode = %v, want 0600", perm)
	}
}

func TestUpdatePreservesModeAndOwnership(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.yaml")
	pepper := []byte("preserve-pepper")

	touch := func(addKey bool) {
		t.Helper()
		if err := Update(usersPath, func(uf *UsersFile) error {
			if addKey {
				key, id, err := Generate("extra")
				if err != nil {
					return err
				}
				uf.Keys = append(uf.Keys, Key{
					ID:         id,
					Name:       "extra",
					SecretHash: FormatHashValue(Hash(pepper, key)),
					Enabled:    true,
					Models:     []string{"*"},
				})
			}
			return nil
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	touch(false) // creates the file at the default 0600

	if err := os.Chmod(usersPath, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeST := before.Sys().(*syscall.Stat_t)

	touch(true) // must preserve mode 0640 and ownership

	after, err := os.Stat(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := after.Mode().Perm(); perm != 0o640 {
		t.Fatalf("users file mode = %v, want 0640 (preserved)", perm)
	}
	afterST := after.Sys().(*syscall.Stat_t)
	if afterST.Uid != beforeST.Uid || afterST.Gid != beforeST.Gid {
		t.Fatalf("users file owner changed: uid/gid = %d/%d, want %d/%d",
			afterST.Uid, afterST.Gid, beforeST.Uid, beforeST.Gid)
	}

	if err := os.Chmod(usersPath, 0o660); err != nil {
		t.Fatal(err)
	}
	touch(false) // mode must be clamped to 0640, never widened
	clamped, err := os.Stat(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := clamped.Mode().Perm(); perm != 0o640 {
		t.Fatalf("users file mode = %v after 0660 input, want clamp to 0640", perm)
	}
}

func TestUpdateRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.yaml")
	hash := func(secret string) string {
		return FormatHashValue(Hash([]byte("pepper-pepper-xx"), secret))
	}
	err := Update(usersPath, func(uf *UsersFile) error {
		uf.Keys = append(uf.Keys,
			Key{ID: "0000000000000000", Name: "a", SecretHash: hash("sk-a-0000000000000000-000000000000000000000000000000000000000000000000000000"), Models: []string{"*"}},
			Key{ID: "0000000000000000", Name: "b", SecretHash: hash("sk-b-0000000000000000-000000000000000000000000000000000000000000000000000000"), Models: []string{"*"}},
		)
		return nil
	})
	if err == nil {
		t.Fatal("duplicate id accepted")
	}
}

func TestLoadUsersRejectsSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "users.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsers(link); err == nil {
		t.Fatal("symlinked users file accepted")
	}
}

func TestLoadUsersRejectsWorldReadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	rawKey, id, err := Generate("t")
	if err != nil {
		t.Fatal(err)
	}
	valid := "version: 1\nkeys:\n  - id: " + id + "\n    name: t\n    secret_hash: " +
		FormatHashValue(Hash([]byte("pepper-pepper-xx"), rawKey)) + "\n    enabled: true\n    models: [\"*\"]\n"

	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsers(path); err == nil {
		t.Fatal("world-readable users file accepted")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsers(path); err != nil {
		t.Fatalf("0640 users file rejected: %v", err)
	}
}

// FIX-14 / PLAN §28: a trailing second YAML document after a `---`
// marker must fail closed instead of being silently discarded by the
// strict decode (which reads only the first document).
func TestLoadUsersRejectsMultiDoc(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	multi := "version: 1\nkeys: []\n---\nversion: 2\nkeys: []\n"
	if err := os.WriteFile(path, []byte(multi), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsers(path); err == nil || !strings.Contains(err.Error(), "multi-document") {
		t.Fatalf("multi-document users file: err = %v, want a multi-document rejection", err)
	}
}

func TestLoadPepperRejectsWorldReadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(path, []byte("sixteen-byte-pepper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPepper(path); err == nil {
		t.Fatal("world-readable pepper accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPepper(path); err != nil {
		t.Fatalf("0600 pepper rejected: %v", err)
	}
}

func TestLoadPepperRejectsShort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(path, []byte("short-pepper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPepper(path); err == nil || !strings.Contains(err.Error(), "pepper too short") {
		t.Fatalf("short pepper: err = %v, want a too-short rejection", err)
	}
}

func TestLoadUsersRejectsAnchorsAndMergeKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	pepper := []byte("pepper-pepper-xx")
	raw1, id1, err := Generate("a")
	if err != nil {
		t.Fatal(err)
	}
	raw2, id2, err := Generate("b")
	if err != nil {
		t.Fatal(err)
	}
	doc := "version: 1\nkeys:\n  - &tpl\n    id: " + id1 + "\n    name: a\n    secret_hash: " +
		FormatHashValue(Hash(pepper, raw1)) + "\n    enabled: true\n    models: [\"*\"]\n" +
		"  - <<: *tpl\n    id: " + id2 + "\n    name: b\n    secret_hash: " +
		FormatHashValue(Hash(pepper, raw2)) + "\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsers(path); err == nil {
		t.Fatal("merge-key hidden wildcard accepted (T-M9)")
	} else if !strings.Contains(err.Error(), "aliases") && !strings.Contains(err.Error(), "anchors") &&
		!strings.Contains(err.Error(), "merge") && !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("unexpected error (want alias/anchor/merge rejection): %v", err)
	}
}

func mustKey(t *testing.T) string {
	t.Helper()
	k, _, err := Generate("test")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// T-X8: negative per-key limits must fail closed at the key-store boundary
// instead of becoming "unlimited" limiters.
func TestValidateUsersRejectsNegativeLimits(t *testing.T) {
	t.Parallel()
	base := Key{
		ID: "0000000000000000", Name: "a",
		SecretHash: FormatHashValue(Hash([]byte("pepper-pepper-xx"), "sk-a-0000000000000000-000000000000000000000000000000000000000000000000000000")),
		Enabled:    true, Models: []string{"*"},
	}
	cases := []KeyLimits{
		{ConcurrentRequests: -1},
		{RequestsPerSecond: -1},
		{Burst: -1},
		{TokensPerHour: -1},
		{TokensPerDay: -1},
	}
	for _, lim := range cases {
		uf := &UsersFile{Version: 1, Keys: []Key{base}}
		uf.Keys[0].Limits = lim
		if err := validateUsers(uf); err == nil {
			t.Fatalf("validateUsers accepted limits %+v", lim)
		}
	}
	// Zero and positive limits are accepted.
	uf := &UsersFile{Version: 1, Keys: []Key{base}}
	uf.Keys[0].Limits = KeyLimits{ConcurrentRequests: 4, RequestsPerSecond: 5, Burst: 10}
	if err := validateUsers(uf); err != nil {
		t.Fatalf("validateUsers rejected valid limits: %v", err)
	}
}

// T-T9: every remaining validateUsers rejection branch must be pinned:
// unsupported version, invalid/empty key id, missing name, malformed
// secret_hash, and an empty models list all fail closed at the key-store
// boundary.
func TestValidateUsersRejectsMalformedKeys(t *testing.T) {
	t.Parallel()
	goodHash := FormatHashValue(Hash([]byte("pepper-pepper-xx"), "sk-a-0000000000000000-000000000000000000000000000000000000000000000000000000"))
	base := Key{ID: "0000000000000000", Name: "a", SecretHash: goodHash, Enabled: true, Models: []string{"*"}}

	cases := []struct {
		name   string
		mutate func(k *Key)
		want   string
	}{
		{
			name: "unsupported version",
			mutate: func(k *Key) {
				_ = k
			},
			want: "version",
		},
		{
			name: "empty id",
			mutate: func(k *Key) {
				k.ID = ""
			},
			want: "invalid id",
		},
		{
			name: "invalid id characters",
			mutate: func(k *Key) {
				k.ID = "AAAAAA1A!"
			},
			want: "invalid id",
		},
		{
			name: "duplicate id",
			mutate: func(k *Key) {
				_ = k
			},
			want: "duplicate key id",
		},
		{
			name: "missing name",
			mutate: func(k *Key) {
				k.Name = ""
			},
			want: "name is required",
		},
		{
			name: "malformed secret_hash",
			mutate: func(k *Key) {
				k.SecretHash = "hmac-sha256:!!!not-base64!!!"
			},
			want: "secret_hash",
		},
		{
			name: "empty models list",
			mutate: func(k *Key) {
				k.Models = nil
			},
			want: "models list must not be empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := base
			tc.mutate(&key)
			uf := &UsersFile{Version: 1, Keys: []Key{key}}
			if tc.name == "duplicate id" {
				uf.Keys = []Key{key, key}
			}
			if tc.name == "unsupported version" {
				uf.Version = 2
			}
			err := validateUsers(uf)
			if err == nil {
				t.Fatalf("validateUsers accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// T-X8: a key with no limits block gets the conservative per-key concurrency
// default at the store boundary, so one key cannot occupy every inflight slot
// by default.
func TestNewStoreAppliesDefaultConcurrency(t *testing.T) {
	t.Parallel()
	pepper := []byte("pepper-pepper-xx")
	idA := strings.Repeat("0", 16)
	idB := strings.Repeat("1", 16)
	rawA := "sk-a-" + idA + "-" + strings.Repeat("0", 64)
	rawB := "sk-b-" + idB + "-" + strings.Repeat("0", 64)
	uf := &UsersFile{Version: 1, Keys: []Key{
		{ID: idA, Name: "a", SecretHash: FormatHashValue(Hash(pepper, rawA)), Enabled: true, Models: []string{"*"}},
		{ID: idB, Name: "b", SecretHash: FormatHashValue(Hash(pepper, rawB)), Enabled: true, Models: []string{"*"}, Limits: KeyLimits{ConcurrentRequests: 2}},
	}}
	store, err := NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}
	k1, err := store.Lookup(rawA)
	if err != nil {
		t.Fatal(err)
	}
	if k1.Limits.ConcurrentRequests != DefaultConcurrentRequests {
		t.Fatalf("no-limits key concurrency = %d, want default %d", k1.Limits.ConcurrentRequests, DefaultConcurrentRequests)
	}
	k2, err := store.Lookup(rawB)
	if err != nil {
		t.Fatal(err)
	}
	if k2.Limits.ConcurrentRequests != 2 {
		t.Fatalf("explicit limit lost: concurrency = %d, want 2", k2.Limits.ConcurrentRequests)
	}
	// The on-disk file must not have been rewritten by the default.
	if uf.Keys[0].Limits.ConcurrentRequests != 0 {
		t.Fatalf("store mutated the users file: %d", uf.Keys[0].Limits.ConcurrentRequests)
	}
}
