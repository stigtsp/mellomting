package auth

import (
	"os"
	"path/filepath"
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

	for i := 0; i < 32; i++ {
		key, id, err := Generate()
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
	cases := []string{
		"",
		"sk-abc_def",
		"mtk_onlyid",
		"mtk_A_B_extra",
		"mtk_7R3F_secretshort!",
		"mtk_7R3F_secret with a space",
		"mtok_7R3F2V_secretsecretsecretsecretsecret",
		"mtk_7R3F2V_secret_secret_split",
	}
	for _, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded", raw)
		}
	}
}

func TestLookupAcceptsValidKey(t *testing.T) {
	key, _, err := Generate()
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
	if _, err := s.Lookup("mtk_9X9X9X_secretnotstored000000000"); err != ErrUnknownKey {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	// Malformed input must also read as unknown, not a format complaint.
	if _, err := s.Lookup("garbage"); err != ErrUnknownKey {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestLookupDisabledAndExpired(t *testing.T) {
	key := "mtk_A1B2C3D4_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := newTestStore(t, key)
	rec := s.byID["A1B2C3D4"]

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
		key, id, err := Generate()
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

func TestUpdateRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.yaml")
	hash := func(secret string) string {
		return FormatHashValue(Hash([]byte("pepper-pepper-xx"), secret))
	}
	err := Update(usersPath, func(uf *UsersFile) error {
		uf.Keys = append(uf.Keys,
			Key{ID: "AAAAAA1A", Name: "a", SecretHash: hash("mtk_AAAAAA1A_xxxxxxxxxxxxxxxxxxxxxxxx"), Models: []string{"*"}},
			Key{ID: "AAAAAA1A", Name: "b", SecretHash: hash("mtk_AAAAAA1A_yyyyyyyyyyyyyyyyyyyyyyyyyy"), Models: []string{"*"}},
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
	rawKey, id, err := Generate()
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

func mustKey(t *testing.T) string {
	t.Helper()
	k, _, err := Generate()
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
		ID: "AAAAAA1A", Name: "a",
		SecretHash: FormatHashValue(Hash([]byte("pepper-pepper-xx"), "mtk_AAAAAA1A_xxxxxxxxxxxxxxxxxxxxxxxx")),
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

// T-X8: a key with no limits block gets the conservative per-key concurrency
// default at the store boundary, so one key cannot occupy every inflight slot
// by default.
func TestNewStoreAppliesDefaultConcurrency(t *testing.T) {
	t.Parallel()
	uf := &UsersFile{Version: 1, Keys: []Key{
		{ID: "AAAAAA1A", Name: "a", SecretHash: FormatHashValue(Hash([]byte("pepper-pepper-xx"), "mtk_AAAAAA1A_xxxxxxxxxxxxxxxxxxxxxxxx")), Enabled: true, Models: []string{"*"}},
		{ID: "BBBBBB2B", Name: "b", SecretHash: FormatHashValue(Hash([]byte("pepper-pepper-xx"), "mtk_BBBBBB2B_yyyyyyyyyyyyyyyyyyyyyyyyyy")), Enabled: true, Models: []string{"*"}, Limits: KeyLimits{ConcurrentRequests: 2}},
	}}
	store, err := NewStore(uf, []byte("pepper-pepper-xx"))
	if err != nil {
		t.Fatal(err)
	}
	k1, err := store.Lookup("mtk_AAAAAA1A_xxxxxxxxxxxxxxxxxxxxxxxx")
	if err != nil {
		t.Fatal(err)
	}
	if k1.Limits.ConcurrentRequests != DefaultConcurrentRequests {
		t.Fatalf("no-limits key concurrency = %d, want default %d", k1.Limits.ConcurrentRequests, DefaultConcurrentRequests)
	}
	k2, err := store.Lookup("mtk_BBBBBB2B_yyyyyyyyyyyyyyyyyyyyyyyyyy")
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
