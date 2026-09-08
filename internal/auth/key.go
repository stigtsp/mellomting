package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Key format and entropy per D18:
//
//	sk-<username>-<keyid>-<secret>
//
// where <username> matches ^[a-z][a-z0-9_]{0,31}$, <keyid> is 8 random bytes
// encoded as exactly 16 lowercase hex characters, and <secret> is 32 random
// bytes (256 bits) encoded as exactly 64 lowercase hex characters. The grammar
// is deliberately strict: no segment is empty and no additional separator or
// suffix is accepted. Multiple keys may share a username because <keyid>
// distinguishes them. The raw key is high-sensitivity: it must never be logged,
// returned, or embedded in error messages.
const (
	// Prefix is the key prefix.
	Prefix = "sk"

	keyIDBytes  = 8  // 8 random bytes -> 16 lowercase hex chars
	secretBytes = 32 // 256 bits of secret entropy

	// MaxRawKeyBytes bounds raw key input (D18): "sk-" + 32 + "-" + 16 +
	// "-" + 64 = 117. Oversized inputs are rejected before parsing
	// segments.
	MaxRawKeyBytes = 117
)

// usernamePattern is the D18 username grammar. It equals `key create --name`
// and is stored as the key's human-visible name.
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidateUsername reports whether username satisfies the D18 grammar. It is
// the operator-facing check used before entropy use or filesystem mutation.
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("invalid name: use 1–32 lowercase letters, digits or underscores, starting with a letter")
	}
	return nil
}

// Generate returns a new API key and its key ID for the validated username.
// The ID and secret are generated independently with crypto/rand.
func Generate(username string) (key, id string, err error) {
	if !usernamePattern.MatchString(username) {
		return "", "", fmt.Errorf("invalid username")
	}
	idb := make([]byte, keyIDBytes)
	if _, err := rand.Read(idb); err != nil {
		return "", "", fmt.Errorf("generate key ID: %w", err)
	}
	id = hex.EncodeToString(idb)

	sb := make([]byte, secretBytes)
	if _, err := rand.Read(sb); err != nil {
		return "", "", fmt.Errorf("generate key secret: %w", err)
	}
	secret := hex.EncodeToString(sb)
	return fmt.Sprintf("%s-%s-%s-%s", Prefix, username, id, secret), id, nil
}

// Parsed is the syntactic breakdown of a raw key. Contains the raw key
// material and the username; keep both out of logs.
type Parsed struct {
	Raw      string
	ID       string
	Username string
}

// Parse validates the key syntax (D18) without revealing which part, if any,
// is malformed. Raw input is bounded before the individual segments are
// parsed.
func Parse(raw string) (Parsed, error) {
	if len(raw) > MaxRawKeyBytes {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	// The grammar contains no "-" inside any segment, so a valid key splits
	// into exactly four parts: prefix, username, keyid, secret.
	parts := strings.Split(raw, "-")
	if len(parts) != 4 {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	prefix, username, id, secret := parts[0], parts[1], parts[2], parts[3]
	if prefix != Prefix ||
		!usernamePattern.MatchString(username) ||
		!isLowerHex(id, keyIDBytes*2) ||
		!isLowerHex(secret, secretBytes*2) {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	return Parsed{Raw: raw, ID: id, Username: username}, nil
}

// isLowerHex reports whether s is exactly n lowercase hexadecimal characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// Hash computes HMAC-SHA-256(pepper, fullKey) per PLAN §27.
func Hash(pepper []byte, fullKey string) []byte {
	return hmacSHA256(pepper, []byte(fullKey))
}

// ParseHashValue decodes a stored "hmac-sha256:<base64>" secret hash.
func ParseHashValue(stored string) ([]byte, error) {
	const prefix = "hmac-sha256:"
	if !strings.HasPrefix(stored, prefix) {
		return nil, fmt.Errorf("unsupported secret_hash encoding")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil || len(raw) != sha256.Size {
		return nil, fmt.Errorf("malformed secret_hash")
	}
	return raw, nil
}

// FormatHashValue encodes a hash for the users file.
func FormatHashValue(h []byte) string {
	return "hmac-sha256:" + base64.RawStdEncoding.EncodeToString(h)
}
