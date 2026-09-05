package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"mellomting/internal/config"
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

// DefaultConcurrentRequests is the per-key in-flight bound applied to a key
// that sets no concurrency limit. PLAN §35 requires that a single key cannot
// occupy the whole process, so "no limits block" must not mean "may hold every
// inflight slot". The default is deliberately far below the global
// max_inflight_requests default (64) so one key can starve nobody by default;
// an operator wanting effectively-unbounded concurrency sets an explicit high
// value. 0 is treated as "unset" because the YAML field uses omitempty and
// cannot distinguish an explicit 0 from an absent block.
const DefaultConcurrentRequests = 8

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

// EmptyUsers is the canonical empty key store written by init and
// systemd provisioning. It is explicitly valid (T-M10) and authenticates
// nobody until an operator creates a key.
const EmptyUsers = `# Mellomting API keys. Managed by ` + "`" + `mellomting key
# create|list|enable|disable|revoke` + "`" + ` — do not edit by hand.
version: 1
keys: []
`

// EmptyUsersBytes returns the canonical empty users-file bytes.
func EmptyUsersBytes() []byte { return []byte(EmptyUsers) }

// LoadUsers reads and validates the users file.
func LoadUsers(path string) (*UsersFile, error) {
	data, err := securefile.Read(path, 1<<20)
	if err != nil {
		return nil, err
	}
	return ParseUsers(data)
}

// ParseUsers validates users-file bytes without touching the filesystem.
func ParseUsers(data []byte) (*UsersFile, error) {
	// The users file gets the same alias/anchor/merge-key rejection as the
	// config file (T-M9): a wildcard ACL must be explicit and easy to spot
	// in review, not hidden inside a `<<:` merge key (PLAN §77).
	if err := config.CheckYAMLTree(data); err != nil {
		return nil, fmt.Errorf("users file: %v", err)
	}
	// A trailing second YAML document must fail, not be silently dropped
	// by the strict decode below (FIX-14): CLI and daemon agree because
	// both go through LoadUsers.
	if err := config.CheckSingleDocument(data); err != nil {
		return nil, fmt.Errorf("users file: %v", err)
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
	// An empty key list is valid (T-M10): a single-key deployment must be
	// able to revoke its last key through the CLI. An empty store serves
	// no keys at all, so the daemon fails closed (every request is 401)
	// until an operator adds a key again.
	seen := make(map[string]bool, len(uf.Keys))
	for i := range uf.Keys {
		k := &uf.Keys[i]
		if !isLowerHex(k.ID, 16) {
			return fmt.Errorf("key %d: invalid id", i)
		}
		if seen[k.ID] {
			return fmt.Errorf("duplicate key id %q", k.ID)
		}
		seen[k.ID] = true
		if k.Name == "" {
			return fmt.Errorf("key %s: name is required", k.ID)
		}
		// D18: the name is the key's username segment; a hand-edited users
		// file with a name outside the grammar fails closed.
		if !usernamePattern.MatchString(k.Name) {
			return fmt.Errorf("key %s: name is not a valid username", k.ID)
		}
		if _, err := ParseHashValue(k.SecretHash); err != nil {
			return fmt.Errorf("key %s: %v", k.ID, err)
		}
		if len(k.Models) == 0 {
			return fmt.Errorf("key %s: models list must not be empty", k.ID)
		}
		if err := validateLimits(&k.Limits); err != nil {
			return fmt.Errorf("key %s: %v", k.ID, err)
		}
	}
	return nil
}

// validateLimits rejects malformed per-key limits. Limits fail closed at the
// key-store boundary: a negative value is a configuration error (0 and above
// are the only valid inputs), never a signal to drop or weaken a bound. In
// particular a negative requests_per_second must never flow into
// limiter.NewBucket as an "unlimited" bucket (T-X8).
func validateLimits(l *KeyLimits) error {
	if l.ConcurrentRequests < 0 {
		return fmt.Errorf("limits.concurrent_requests must be >= 0, got %d", l.ConcurrentRequests)
	}
	if l.RequestsPerSecond < 0 {
		return fmt.Errorf("limits.requests_per_second must be >= 0, got %v", l.RequestsPerSecond)
	}
	if l.Burst < 0 {
		return fmt.Errorf("limits.burst must be >= 0, got %d", l.Burst)
	}
	if l.TokensPerHour < 0 {
		return fmt.Errorf("limits.tokens_per_hour must be >= 0, got %d", l.TokensPerHour)
	}
	if l.TokensPerDay < 0 {
		return fmt.Errorf("limits.tokens_per_day must be >= 0, got %d", l.TokensPerDay)
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
	if err := ValidatePepper([]byte(pepper)); err != nil {
		return nil, err
	}
	return []byte(pepper), nil
}

// ValidatePepper checks pepper bytes without writing them to disk.
func ValidatePepper(pepper []byte) error {
	if len(pepper) < 16 {
		return fmt.Errorf("pepper too short (minimum 16 bytes)")
	}
	return nil
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
		k := uf.Keys[i] // copy: normalization must not rewrite the on-disk file
		if k.Limits.ConcurrentRequests == 0 {
			// No concurrency limit set: apply the conservative default
			// so a single key cannot occupy every inflight slot (PLAN
			// §35, T-X8). The store is the boundary between the file and
			// enforcement, so both the daemon and direct callers observe
			// the default.
			k.Limits.ConcurrentRequests = DefaultConcurrentRequests
		}
		s.byID[k.ID] = &k
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
	// D18: a matching hash must also carry the stored key's username.
	if subtle.ConstantTimeCompare([]byte(parsed.Username), []byte(rec.Name)) != 1 {
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
// replacement (PLAN §29.1): lock, parse, validate, then replace through
// securefile, which owns the temp-file, fsync, and rename mechanics.
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

	// PLAN §29.1: preserve the existing file's ownership and its mode
	// clamped to 0640 (never widened), so a privileged `key create`
	// cannot leave a 0600 root:root file the daemon can no longer read.
	// A fresh file defaults to 0600.
	return securefile.ReplacePreservingOwner(path, 0o600, 0o640, func(w io.Writer) error {
		_, err := w.Write(out)
		return err
	})
}
