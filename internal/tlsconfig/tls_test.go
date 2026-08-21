package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedTLS writes a self-signed certificate for DNS name
// "localhost" and its key into dir and returns their paths.
func writeSelfSignedTLS(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	derCert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	derKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	certOut, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derCert}); err != nil {
		t.Fatal(err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	keyOut, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PKCS8 PRIVATE KEY", Bytes: derKey}); err != nil {
		t.Fatal(err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedTLS(t, dir)

	cfg, err := Files(certPath, keyPath)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want %x (secure default, PLAN §97)", cfg.MinVersion, tls.VersionTLS12)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
	}
}

func TestFilesHandshake(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedTLS(t, dir)
	cfg, err := Files(certPath, keyPath)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestFilesMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, keyPath := writeSelfSignedTLS(t, dir)
	if _, err := Files(filepath.Join(dir, "absent.pem"), keyPath); err == nil {
		t.Fatal("missing cert accepted")
	}
	certPath, _ := writeSelfSignedTLS(t, dir)
	if _, err := Files(certPath, filepath.Join(dir, "absent-key.pem")); err == nil {
		t.Fatal("missing key accepted")
	}
}

func TestFilesKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	dirB := filepath.Join(dir, "b")
	if err := os.Mkdir(dirB, 0o700); err != nil {
		t.Fatal(err)
	}
	certA, _ := writeSelfSignedTLS(t, dir)
	_, keyB := writeSelfSignedTLS(t, dirB)
	if _, err := Files(certA, keyB); err == nil {
		t.Fatal("certificate/key pair mismatch accepted")
	}
}

func TestFilesSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedTLS(t, dir)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(certPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(link, keyPath); err == nil {
		t.Fatal("symlink final component accepted (PLAN §28)")
	}
}

func TestFilesRejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedTLS(t, dir)
	// A world-readable private key is refused fail-closed (T-M8).
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(certPath, keyPath); err == nil {
		t.Fatal("world-readable TLS key accepted (T-M8)")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// A world-readable certificate is likewise refused.
	if err := os.Chmod(certPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(certPath, keyPath); err == nil {
		t.Fatal("world-readable TLS certificate accepted (T-M8)")
	}
	if err := os.Chmod(certPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(certPath, keyPath); err != nil {
		t.Fatalf("0600 TLS files rejected: %v", err)
	}
}
