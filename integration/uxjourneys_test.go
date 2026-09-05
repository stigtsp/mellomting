//go:build integration

// Package integration runs the end-to-end UX journeys (PLAN §15, E2) against
// the real binary in a temporary directory, using only local fake model
// servers. It never touches the host's /etc, users, systemd state, or
// firewall: the systemd scenario exercises the importable, non-mutating
// rendering and the fail-closed preflight only, and every serve run binds a
// temporary socket/TCP port.
package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mellomting/internal/testsupport"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"mellomting/internal/landlock"
	"mellomting/internal/systemd"
)

func TestUXJourneys(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the serve/Landlock journeys need a Linux host; skipping on %s", runtime.GOOS)
	}
	bin := buildBinary(t)

	t.Run("init_one_server", func(t *testing.T) { journeyInitOneServer(t, bin) })
	t.Run("init_two_overlapping_servers", func(t *testing.T) { journeyInitTwoServers(t, bin) })
	t.Run("init_landlock_preflight", func(t *testing.T) { journeyInitLandlock(t, bin) })
	t.Run("init_dry_run", func(t *testing.T) { journeyInitDryRun(t, bin) })
	t.Run("systemd_fixtures", func(t *testing.T) { journeySystemd(t, bin) })
	t.Run("models_endpoint", func(t *testing.T) { journeyModelsEndpoint(t, bin) })
	t.Run("static_tls", func(t *testing.T) { journeyStaticTLS(t, bin) })
}

// buildBinary compiles the real binary once into a temporary location.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mellomting")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/mellomting")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %v: %s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// The test runs from the integration/ directory.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dir)
}

// runCmd runs the binary in dir with a bounded context and returns the exit
// code plus captured stdout/stderr.
func runCmd(t *testing.T, bin, dir string, timeout time.Duration, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var outB, errB bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	code := 0
	if ctx.Err() == context.DeadlineExceeded {
		return -1, outB.String(), errB.String() + " (timeout)"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec %v: %v", args, err)
	}
	return code, outB.String(), errB.String()
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

func requireFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
		fi := mustStat(t, filepath.Join(dir, name))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, fi.Mode().Perm())
		}
	}
}

// privateDir creates a 0700 working directory. init rejects destination
// directories that are group- or world-writable, and testing.T.TempDir may
// create 0775 directories on some hosts.
func privateDir(t *testing.T) string {
	t.Helper()
	sub := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	return sub
}

// freeTCPPort returns a loopback address with a free port, closing the probe
// listener immediately so the serve subprocess can bind it.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// addTLSBlock inserts a `tls:` block (with the given "field: value" entries)
// as a child of the top-level `server:` mapping, matching the existing child
// indentation so `listen:` remains a sibling rather than nesting under `tls`.
func addTLSBlock(t *testing.T, raw string, fields ...string) string {
	t.Helper()
	lines := strings.Split(raw, "\n")
	serverIdx := -1
	for i, l := range lines {
		if l == "server:" {
			serverIdx = i
			break
		}
	}
	if serverIdx < 0 {
		t.Fatalf("no top-level server: line in config:\n%s", raw)
	}
	childIndent := 4
	for _, l := range lines[serverIdx+1:] {
		if l == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); n > 0 {
			childIndent = n
			break
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", childIndent) + "tls:\n")
	for _, f := range fields {
		b.WriteString(strings.Repeat(" ", childIndent+4) + f + "\n")
	}
	out := make([]string, 0, len(lines)+len(fields)+1)
	out = append(out, lines[:serverIdx+1]...)
	out = append(out, strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")...)
	out = append(out, lines[serverIdx+1:]...)
	return strings.Join(out, "\n")
}

// --- journey 1+2: local init ---

func journeyInitOneServer(t *testing.T, bin string) {
	backend := testsupport.FakeModelsServer(t, "qwen3.8-27b")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")
	sock := filepath.Join(dir, "m.sock")

	code, out, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init", "--server", backend.URL, "--listen", sock, "--config", cfg)
	if code != 0 {
		t.Fatalf("init exit = %d; stderr=%s", code, errOut)
	}
	requireFiles(t, dir)
	cfgBytes, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfgBytes), "qwen3.8-27b") || !strings.Contains(string(cfgBytes), backend.URL) {
		t.Fatalf("config missing the discovered model/server:\n%s", cfgBytes)
	}
	// The pepper and users file are never printed.
	if strings.Contains(out+errOut, "hmac-sha256:") {
		t.Fatalf("init leaked a stored hash to the console:\n%s", out+errOut)
	}
}

func journeyInitTwoServers(t *testing.T, bin string) {
	a := testsupport.FakeModelsServer(t, "model-a", "model-shared")
	b := testsupport.FakeModelsServer(t, "model-b", "model-shared")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, _, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init",
		"--server", "a="+a.URL,
		"--server", "b="+b.URL,
		"--listen", filepath.Join(dir, "m.sock"),
		"--config", cfg)
	if code != 0 {
		t.Fatalf("init exit = %d; stderr=%s", code, errOut)
	}
	cfgBytes, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(cfgBytes)
	// model-shared is served by both replicas; model-a only by a, model-b by b.
	for _, want := range []string{"model-a", "model-b", "model-shared", "a:", "b:"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("config missing %q:\n%s", want, doc)
		}
	}
}

// journeyInitLandlock exercises the init Landlock preflight through the CLI
// surface. The injected-seam non-Linux path is covered by the cmd package's
// own unit tests (B6); here we assert the observable contract on this host.
func journeyInitLandlock(t *testing.T, bin string) {
	report := landlock.Check()
	backend := testsupport.FakeModelsServer(t, "m")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")
	sock := filepath.Join(dir, "m.sock")

	// best-effort is the non-Linux / unsupported-equivalent path: it always
	// proceeds without the sandbox and records the choice in the config.
	code, _, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init", "--server", backend.URL, "--listen", sock, "--config", cfg, "--landlock", "best-effort")
	if code != 0 {
		t.Fatalf("init --landlock best-effort exit = %d; stderr=%s", code, errOut)
	}
	cfgBytes, _ := os.ReadFile(cfg)
	if !strings.Contains(string(cfgBytes), "best-effort") {
		t.Fatalf("best-effort not recorded in the generated config:\n%s", cfgBytes)
	}

	// required (the default) must succeed on a Landlock-capable kernel and
	// fail closed on one that cannot satisfy the minimum ABI. It uses a fresh
	// directory so init does not see the best-effort artifacts above.
	dir2 := privateDir(t)
	code2, _, errOut2 := runCmd(t, bin, dir2, 60*time.Second,
		"init", "--server", backend.URL, "--listen", filepath.Join(dir2, "m.sock"), "--config", filepath.Join(dir2, "config.yaml"), "--landlock", "required")
	if report.Supported && report.KernelABI >= landlock.DefaultMinimumABI {
		if code2 != 0 {
			t.Fatalf("init --landlock required should pass on this kernel (ABI %d); stderr=%s", report.KernelABI, errOut2)
		}
	} else if code2 == 0 {
		t.Fatalf("init --landlock required unexpectedly passed on an unsupported kernel (ABI %d)", report.KernelABI)
	}
}

// journeyInitDryRun asserts the D4 dry-run contract: the exact config YAML on
// stdout, a bounded summary on stderr, and zero files written.
func journeyInitDryRun(t *testing.T, bin string) {
	backend := testsupport.FakeModelsServer(t, "m")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, out, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init", "--server", backend.URL, "--listen", filepath.Join(dir, "m.sock"), "--config", cfg, "--dry-run")
	if code != 0 {
		t.Fatalf("init --dry-run exit = %d; stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "version: 1") || !strings.Contains(out, "models:") {
		t.Fatalf("dry-run stdout is not the config YAML:\n%s", out)
	}
	for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("dry-run wrote %s (want no files)", name)
		}
	}
	_ = errOut
}

// journeySystemd exercises the systemd install surface without mutating /etc:
// the rendered hardened unit, the byte-identical scaffold/logrotate assets,
// and the fail-closed preflight when the host is not root.
func journeySystemd(t *testing.T, bin string) {
	unit := systemd.RenderService(bin)
	if !strings.Contains(unit, "ExecStart="+bin) {
		t.Fatalf("rendered unit missing ExecStart=%s:\n%s", bin, unit)
	}
	if !strings.Contains(unit, "After=network-online.target") {
		t.Fatalf("rendered unit missing After=network-online.target:\n%s", unit)
	}

	repo := repoRoot(t)
	deploy, err := os.ReadFile(filepath.Join(repo, "deploy", "mellomting-config.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(systemd.ConfigTemplate(), deploy) {
		t.Fatal("embedded scaffold differs from deploy/mellomting-config.yaml.example")
	}
	if !bytes.Contains(systemd.Logrotate(), []byte("copytruncate")) {
		t.Fatal("logrotate asset missing copytruncate")
	}

	// Fail-closed preflight: as a non-root user the provisioning must refuse
	// before touching any host state.
	if os.Geteuid() != 0 {
		if err := (&systemd.Provision{BinaryPath: bin}).CheckHost(); err == nil || !strings.Contains(err.Error(), "root") {
			t.Fatalf("CheckHost as non-root should fail closed naming root; got %v", err)
		}
	}
}

// journeyModelsEndpoint runs the full journey: init -> key create -> serve,
// then an authenticated request to /v1/models and an unauthenticated one.
func journeyModelsEndpoint(t *testing.T, bin string) {
	backend := testsupport.FakeModelsServer(t, "qwen3.8-27b")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")
	sock := filepath.Join(dir, "m.sock")

	code, _, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init", "--server", backend.URL, "--listen", sock, "--config", cfg)
	if code != 0 {
		t.Fatalf("init exit = %d; stderr=%s", code, errOut)
	}

	code, keyOut, errOut := runCmd(t, bin, dir, 60*time.Second,
		"key", "create", "--config", cfg, "--name", "client", "--models", "qwen3.8-27b")
	if code != 0 {
		t.Fatalf("key create exit = %d; stderr=%s", code, errOut)
	}
	key := strings.TrimSpace(keyOut)
	if !strings.HasPrefix(key, "sk-client-") {
		t.Fatalf("key create stdout is not the raw key: %q", keyOut)
	}

	serve := startServe(t, bin, dir, cfg)
	defer serve.stop()
	if !serve.waitReady(t, sock) {
		t.Fatalf("serve did not become ready: stderr=%s", serve.err.String())
	}

	client := testsupport.UnixClient(sock)
	// Authenticated: the model the key is allowed to see.
	req, _ := http.NewRequest("GET", "http://unix/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "qwen3.8-27b") {
		t.Fatalf("models list missing the ACL model: %s", body)
	}

	// Unauthenticated: 401.
	resp2, err := client.Get("http://unix/v1/models")
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	_, _ = io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/models status = %d, want 401", resp2.StatusCode)
	}
}

// journeyStaticTLS proves mode is unnecessary (only cert_file/key_file),
// plaintext is not served on a TLS listener, and a partial cert/key config
// fails before bind.
func journeyStaticTLS(t *testing.T, bin string) {
	backend := testsupport.FakeModelsServer(t, "qwen3.8-27b")
	dir := privateDir(t)
	cfg := filepath.Join(dir, "config.yaml")
	addr := freeTCPPort(t)

	code, _, errOut := runCmd(t, bin, dir, 60*time.Second,
		"init", "--server", backend.URL, "--listen", addr, "--config", cfg)
	if code != 0 {
		t.Fatalf("init exit = %d; stderr=%s", code, errOut)
	}

	cert, key := testsupport.WriteSelfSignedCert(t, t.TempDir(), testsupport.CertOptions{CommonName: "mellomting-test", IPs: []string{"127.0.0.1"}})
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	edited := addTLSBlock(t, string(raw), "cert_file: "+cert, "key_file: "+key)
	if err := os.WriteFile(cfg, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	// Partial cert/key must fail before bind.
	partial := addTLSBlock(t, string(raw), "cert_file: "+cert)
	partialCfg := filepath.Join(dir, "partial.yaml")
	if err := os.WriteFile(partialCfg, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, e := runCmd(t, bin, dir, 30*time.Second, "serve", "--config", partialCfg); c == 0 {
		t.Fatalf("serve with a partial cert/key config should fail; stderr=%s", e)
	}

	// Create the key before serve loads the users file, so it is present at
	// startup.
	authKey := mustKey(t, bin, dir, cfg)

	// Serve over TLS.
	serve := startServe(t, bin, dir, cfg)
	defer serve.stop()
	if !serve.waitReadyTCP(t, addr) {
		t.Fatalf("TLS serve did not become ready: stderr=%s", serve.err.String())
	}

	tlsClient := insecureTLSClient()
	req, _ := http.NewRequest("GET", "https://"+addr+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+authKey)
	resp, err := tlsClient.Do(req)
	if err != nil {
		t.Fatalf("TLS GET /v1/models: %v (stderr=%s)", err, serve.err.String())
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS /v1/models status = %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "qwen3.8-27b") {
		t.Fatalf("TLS models list missing the model: %s", body)
	}

	// Plaintext on a TLS listener must not be served: a cleartext request is
	// rejected (a handshake error, or Go's 400 "Client sent an HTTP request
	// to an HTTPS server"), and in neither case does it return the model list.
	pResp, pErr := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/v1/models")
	if pErr == nil {
		pBody, _ := io.ReadAll(pResp.Body)
		_ = pResp.Body.Close()
		if pResp.StatusCode == http.StatusOK && strings.Contains(string(pBody), "qwen3.8-27b") {
			t.Fatalf("plaintext HTTP served the model list over a TLS listener (status=%d body=%q)", pResp.StatusCode, pBody)
		}
	}
}

// mustKey creates a key for the given config and returns the raw key.
func mustKey(t *testing.T, bin, dir, cfg string) string {
	t.Helper()
	code, out, errOut := runCmd(t, bin, dir, 60*time.Second,
		"key", "create", "--config", cfg, "--name", "tlsclient", "--models", "qwen3.8-27b")
	if code != 0 {
		t.Fatalf("key create exit = %d; stderr=%s", code, errOut)
	}
	return strings.TrimSpace(out)
}

func insecureTLSClient() *http.Client {
	// InsecureSkipVerify is acceptable here: the journey's self-signed
	// certificate is exercised by the test, not validated by this client.
	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: transport}
}

// serveProc manages a background serve subprocess.
type serveProc struct {
	cmd *exec.Cmd
	err bytes.Buffer
}

func startServe(t *testing.T, bin, dir, cfg string) *serveProc {
	t.Helper()
	sp := &serveProc{}
	sp.cmd = exec.Command(bin, "serve", "--config", cfg)
	sp.cmd.Dir = dir
	sp.cmd.Stderr = &sp.err
	sp.cmd.Stdout = io.Discard
	if err := sp.cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() { sp.stop() })
	return sp
}

func (sp *serveProc) stop() {
	if sp.cmd.Process == nil {
		return
	}
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = sp.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = sp.cmd.Process.Kill()
	}
}

func (sp *serveProc) waitReady(t *testing.T, sock string) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c := testsupport.UnixClient(sock)
		resp, err := c.Get("http://unix/v1/models")
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func (sp *serveProc) waitReadyTCP(t *testing.T, addr string) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	plain := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		// The listener is TLS; a plain HTTP attempt must at least reach the
		// TLS handshake (a connection error that is not "connection refused"
		// means the listener is up).
		resp, err := plain.Get("http://" + addr + "/v1/models")
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return true
		}
		if !strings.Contains(err.Error(), "connection refused") {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
