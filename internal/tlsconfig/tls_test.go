package tlsconfig

import (
	"crypto/tls"
	"mellomting/internal/testsupport"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})

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
	certPath, keyPath := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
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
	_, keyPath := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
	if _, err := Files(filepath.Join(dir, "absent.pem"), keyPath); err == nil {
		t.Fatal("missing cert accepted")
	}
	certPath, _ := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
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
	certA, _ := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
	_, keyB := testsupport.WriteSelfSignedCert(t, dirB, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
	if _, err := Files(certA, keyB); err == nil {
		t.Fatal("certificate/key pair mismatch accepted")
	}
}

func TestFilesSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(certPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(link, keyPath); err == nil {
		t.Fatal("symlink final component accepted (PLAN §28)")
	}
}

func TestFilesRejectsWorldReadableKeyAcceptsWorldReadableCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
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
	// A world-readable certificate is public data and is accepted
	// (FIX-06/M31): certbot issues fullchain.pem as 0644.
	if err := os.Chmod(certPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(certPath, keyPath); err != nil {
		t.Fatalf("world-readable TLS certificate rejected (FIX-06/M31): %v", err)
	}
	if err := os.Chmod(certPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(certPath, keyPath); err != nil {
		t.Fatalf("0600 TLS files rejected: %v", err)
	}
}

// FIX-06/M31: the certificate is public data, so a world-readable mode is
// accepted — but the final symlink and regular-file guards must still
// hold for it.
func TestFilesPublicCertStillSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := testsupport.WriteSelfSignedCert(t, dir, testsupport.CertOptions{CommonName: "localhost", DNSNames: []string{"localhost"}})
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(certPath, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(certPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(link, link); err == nil {
		t.Fatal("symlink final component accepted for a public cert (PLAN §28)")
	}
}
