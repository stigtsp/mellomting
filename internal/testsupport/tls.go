package testsupport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CertOptions states which names a test certificate must cover. They are
// explicit because the three copies this replaces each covered a
// different subset — one DNS-only, one IP-only, one both — so a caller
// that needs hostname verification and a caller that needs IP
// verification were silently exercising different things.
type CertOptions struct {
	CommonName string
	DNSNames   []string
	IPs        []string
}

// WriteSelfSignedCert writes a self-signed certificate and its PKCS#8
// key into a fresh subdirectory of dir and returns their absolute paths,
// so repeated calls with the same dir do not clobber each other. Use the
// returned paths: the files are not at dir/tls.crt and dir/tls.key.
func WriteSelfSignedCert(t *testing.T, dir string, opts CertOptions) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ips := make([]net.IP, 0, len(opts.IPs))
	for _, s := range opts.IPs {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test certificate IP %q", s)
		}
		ips = append(ips, ip)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: opts.CommonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     opts.DNSNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	derKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	// Unique per call: two certificates written into one directory must
	// not clobber each other, which fixed names would do silently.
	certDir, err := os.MkdirTemp(dir, "cert-")
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(certDir, "tls.crt")
	keyPath = filepath.Join(certDir, "tls.key")
	writePEM(t, certPath, 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	// The private key is secret-bearing: securefile refuses a
	// group- or world-readable mode (T-M8).
	writePEM(t, keyPath, 0o600, &pem.Block{Type: "PRIVATE KEY", Bytes: derKey})
	return certPath, keyPath
}

func writePEM(t *testing.T, path string, mode os.FileMode, block *pem.Block) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
