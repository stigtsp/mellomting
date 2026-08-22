package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
)

// Key format and entropy per PLAN §25:
//
//	mtk_<key-id>_<secret>
//
// with a random (non-sequential) key ID and a 256-bit secret from
// crypto/rand. Both fields are base32 over a Crockford-style alphabet
// (A-Z minus I,L,O,U; digits 2-9) that never contains the "_" delimiter,
// so "mtk_<id>_<secret>" always splits into exactly three parts and is
// safe to quote in logs, URLs, and headers. The raw key is
// high-sensitivity: it must never be logged, returned, or embedded in
// error messages.
const (
	// Prefix is the key prefix.
	Prefix = "mtk"

	keyIDBytes  = 5  // 5 random bytes -> 8 base32 chars (~40 bits of ID space)
	secretBytes = 32 // 256 bits of secret entropy
)

// keyIDAlphabet is Crockford-style base32 without ambiguous characters.
// It is reused to encode the secret so the key body stays delimiter-safe.
var keyIDAlphabet = base32.NewEncoding("ABCDEFGHJKLMNPQRSTUVWXYZ23456789").WithPadding(base32.NoPadding)

// Generate returns a new API key and its key ID.
func Generate() (key, id string, err error) {
	idb := make([]byte, keyIDBytes)
	if _, err := rand.Read(idb); err != nil {
		return "", "", fmt.Errorf("generate key ID: %w", err)
	}
	id = keyIDAlphabet.EncodeToString(idb)

	sb := make([]byte, secretBytes)
	if _, err := rand.Read(sb); err != nil {
		return "", "", fmt.Errorf("generate key secret: %w", err)
	}
	return Prefix + "_" + id + "_" + keyIDAlphabet.EncodeToString(sb), id, nil
}

// Parsed is the syntactic breakdown of a raw key. Contains the raw key
// material; keep it out of logs.
type Parsed struct {
	Raw string
	ID  string
}

// Parse validates the key syntax (step 1-2 of PLAN §27) without
// revealing which part, if any, was malformed.
func Parse(raw string) (Parsed, error) {
	parts := strings.Split(raw, "_")
	if len(parts) != 3 {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	if parts[0] != Prefix {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	id, secret := parts[1], parts[2]
	if len(id) < 2 || len(id) > 12 || !isKeyID(id) {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	if len(secret) < 16 || len(secret) > 128 || !isSecret(secret) {
		return Parsed{}, fmt.Errorf("invalid API key format")
	}
	return Parsed{Raw: raw, ID: id}, nil
}

func isKeyID(s string) bool {
	for _, r := range s {
		if !isIDChar(r) {
			return false
		}
	}
	return true
}

func isIDChar(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func isSecret(s string) bool {
	for _, r := range s {
		if !(isIDChar(r) || r == '-' || r == '_') {
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
