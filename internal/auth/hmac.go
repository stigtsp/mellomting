package auth

import (
	"crypto/hmac"
	"crypto/sha256"
)

// hmacSHA256 computes HMAC-SHA-256(key, msg).
func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}
