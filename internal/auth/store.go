package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"mellomting/internal/securefile"
)

// Errors returned by Store.Lookup. The distinction between "unknown" and
// "known but disabled/expired" is internal; callers map them to HTTP
// codes but never echo the reason beyond the chosen message.
var (
	ErrUnknownKey = fmt.Errorf("unknown API key")
	ErrDisabled   = fmt.Errorf("API key is disabled")
	ErrExpired    = fmt.Errorf("API key is expired")
)

// KeyLimits is the per-key rate/concurrency and token budget from PLAN §26.
// The values are stored and surfaced by the CLI now; enforcement lands in
// later phases (PLAN §34-40).
type KeyLimits struct {
	RequestsPerSecond  float64 `yaml:"requests_per_second"`
	Burst              int     `yaml:"burst"`
	ConcurrentRequests int     `yaml:"concurrent_requests"`
	TokensPerHour      int64   `yaml:"tokens_per_hour"`
	TokensPerDay       int64   `yaml:"tokens_per_day"`
}

// Key is one record of the users file (PLAN §26).
type Key struct {
	ID         string     `yaml:"id"`
	Name       string     `yaml:"name"`
	SecretHash string     `yaml:"secret_hash"`
	Enabled    bool       `yaml:"enabled"`
	ExpiresAt  *time.Time `yaml:"expires_at"`
	Models     []string   `yaml:"models"`
	Limits     KeyLimits  `yaml:"limits,omitempty"`
}

// Allows reports whether the key may use the public model (PLAN §31).
// The "*" wildcard is explicit and easy to spot in review (PLAN §77).
func (k *Key) Allows(model string) bool {
	for _, m := range k.Models {
		if m == model || m == "*" {
			return true
		}
	}
	return false
}

// UsersFile is the on-disk key store (PLAN §26, §77). The daemon treats
// it as read-only.
type UsersFile struct {
	Version int   `yaml:"version"`
	Keys    []Key `yaml:"keys"`
}

// LoadUsers reads and validates the users file.
func LoadUsers(path string) (*UsersFile, error) {
	data, err := securefile.Read(path, 1<<20)
	if err != nil {
		return nil, err
	}
	var uf UsersFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&uf); err != nil {
		return nil, err
	}
	if err := validateUsers(&uf); err != nil {
		return nil, err
	}
	return &uf, nil
}

func validateUsers(uf *UsersFile) error {
	if uf.Version != 1 {
		return fmt.Errorf("users file version %d is not supported (want 1)", uf.Version)
	}
	if len(uf.Keys) == 0 {
		return fmt.Errorf("users file needs at least one key")
	}
	seen := make(map[string]bool, len(uf.Keys))
	for i := range uf.Keys {
		k := &uf.Keys[i]
		if k.ID == "" || !isKeyID(k.ID) {
			return fmt.Errorf("key %d: invalid id", i)
		}
		if seen[k.ID] {
			return fmt.Errorf("duplicate key id %q", k.ID)
		}
		seen[k.ID] = true
		if k.Name == "" {
			return fmt.Errorf("key %s: name is required", k.ID)
		}
		if _, err := ParseHashValue(k.SecretHash); err != nil {
			return fmt.Errorf("key %s: %v", k.ID, err)
		}
		if len(k.Models) == 0 {
			return fmt.Errorf("key %s: models list must not be empty", k.ID)
		}
	}
	return nil
}

// LoadPepper reads the HMAC pepper file (PLAN §27). A single trailing
// newline is tolerated. An empty pepper is rejected: fail closed.
func LoadPepper(path string) ([]byte, error) {
	data, err := securefile.Read(path, 4096)
	if err != nil {
		return nil, err
	}
	pepper := strings.TrimRight(string(data), "\r\n")
	if len(pepper) < 16 {
		return nil, fmt.Errorf("pepper too short (minimum 16 bytes)")
	}
	return []byte(pepper), nil
}

// Store is the in-memory, read-only key table used by the daemon.
type Store struct {
	byID      map[string]*Key
	pepper    []byte
	dummyHash []byte
	now       func() time.Time
}

// NewStore builds a Store from a validated users file and pepper.
func NewStore(uf *UsersFile, pepper []byte) (*Store, error) {
	if len(pepper) == 0 {
		return nil, fmt.Errorf("pepper must not be empty")
	}
	s := &Store{
		byID:   make(map[string]*Key, len(uf.Keys)),
		pepper: pepper,
		now:    time.Now,
	}
	for i := range uf.Keys {
		s.byID[uf.Keys[i].ID] = &uf.Keys[i]
	}
	// A per-process random dummy hash equalises timing for unknown key
	// IDs (PLAN §27 step: "perform a dummy HMAC operation").
	s.dummyHash = make([]byte, 32)
	if _, err := rand.Read(s.dummyHash); err != nil {
		return nil, err
	}
	return s, nil
}

// Lookup authenticates a raw key and returns the enabled, unexpired key
// record (PLAN §27). It never returns the raw key or its secret.
func (s *Store) Lookup(rawKey string) (*Key, error) {
	parsed, err := Parse(rawKey)
	if err != nil {
		// Still run a dummy HMAC to equalise timing across malformed
		// versus unknown inputs.
		hmacSHA256(s.pepper, []byte(dummyHashInput(s)))
		return nil, ErrUnknownKey
	}

	rec, known := s.byID[parsed.ID]
	var stored []byte
	if known {
		stored, err = ParseHashValue(rec.SecretHash)
		if err != nil {
			// Config invariant; fail closed for this request.
			return nil, ErrUnknownKey
		}
	} else {
		stored = s.dummyHash
	}

	got := Hash(s.pepper, parsed.Raw)
	if !known || subtle.ConstantTimeCompare(got, stored) != 1 {
		return nil, ErrUnknownKey
	}
	if !rec.Enabled {
		return nil, ErrDisabled
	}
	if rec.ExpiresAt != nil && s.now().After(*rec.ExpiresAt) {
		return nil, ErrExpired
	}
	return rec, nil
}

func dummyHashInput(s *Store) string {
	return Prefix + "_0000_" + string(s.dummyHash)
}

// Update mutates the users file under an exclusive lock with an atomic
// replacement (PLAN §29.1): lock, parse/validate, temp file in the same
// directory, mode 0600, write, fsync file, rename, fsync directory.
func Update(path string, fn func(*UsersFile) error) error {
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	uf, err := LoadUsers(path)
	if err != nil {
		if os.IsNotExist(err) {
			uf = &UsersFile{Version: 1}
		} else {
			return err
		}
	}
	if err := fn(uf); err != nil {
		return err
	}
	if err := validateUsers(uf); err != nil {
		return err
	}

	out, err := yaml.Marshal(uf)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mellomting-users-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""

	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
