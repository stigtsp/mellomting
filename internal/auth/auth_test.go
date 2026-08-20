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

func mustKey(t *testing.T) string {
	t.Helper()
	k, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
