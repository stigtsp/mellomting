// Package tlsconfig builds the static TLS configuration for the ingress
// listener (PLAN §67, §97). The certificate and key are loaded once at
// startup, before the sandbox is applied (PLAN §57 step 9); a certificate
// reload requires a process restart in v1.
package tlsconfig

import (
	"crypto/tls"
	"fmt"

	"mellomting/internal/securefile"
)

// Files reads certFile and keyFile and returns the *tls.Config for the
// ingress listener. The files are opened without following a final symlink
// and must be regular files (PLAN §28); the file descriptors are closed
// before returning, so no startup FD survives into the confined phase
// (PLAN §59).
//
// Secure defaults (PLAN §97): the protocol minimum is pinned to TLS 1.2;
// cipher suites are Go's built-in secure set, with TLS 1.3 preferred when
// negotiated. The certificate is fixed for the process lifetime.
func Files(certFile, keyFile string) (*tls.Config, error) {
	cert, err := securefile.Read(certFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("tls cert %q: %w", certFile, err)
	}
	key, err := securefile.Read(keyFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("tls key %q: %w", keyFile, err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("tls key pair is invalid: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}, nil
}
