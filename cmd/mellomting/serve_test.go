package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/landlock"
)

// unixHTTPClient dials a pathname Unix socket for HTTP requests.
func unixHTTPClient(sock string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func getURL(t *testing.T, client *http.Client, url, authValue string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authValue != "" {
		req.Header.Set("Authorization", "Bearer "+authValue)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func postJSON(t *testing.T, client *http.Client, url, authValue, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authValue != "" {
		req.Header.Set("Authorization", "Bearer "+authValue)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// fakeChatBackend serves /v1/chat/completions with JSON or SSE.
func fakeChatBackend(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		if env.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				`data: {"id":"e2e-1","choices":[{"delta":{"content":"He"}}]}` + "\n\n" +
					`event: mellomting.test` + "\n" +
					`data: {"note":"passthru"}` + "\n\n" +
					`data: [DONE]` + "\n\n",
			))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"e2e-1","object":"chat.completion","model":%q}`, env.Model)))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// slowChatBackend completes non-streaming requests immediately but holds
// streaming requests open: it flushes one chunk then waits on release
// before finishing. It keeps a handler alive across shutdown so a drain
// is observable (T-M11).
func slowChatBackend(t *testing.T, release chan struct{}) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		if !env.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"slow-1","object":"chat.completion","model":%q}`, env.Model)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if fl, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(`data: {"id":"slow-1","choices":[{"delta":{"content":"H"}}]}` + "\n\n"))
			fl.Flush()
		}
		<-release
		_, _ = w.Write([]byte(`data: [DONE]` + "\n\n"))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// shortTempDir returns a temp directory whose absolute path is short,
// so Unix socket paths built from it stay under the sun_path limit
// (~104 bytes on macOS) even when $TMPDIR is long (FIX-29). It prefers
// a short, writable base (/tmp) and falls back to t.TempDir() when that
// is unavailable, so the suite still runs in restricted environments.
func shortTempDir(t *testing.T) string {
	t.Helper()
	if d, err := os.MkdirTemp("/tmp", "mtt"); err == nil {
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	return t.TempDir()
}

// serveFixture wires a temp config + users/pepper + fake backend.
// landlockMode is the security.landlock.mode of the written config.
func serveFixture(t *testing.T, landlockMode string) (bin, cfgPath, sock string, key string) {
	t.Helper()
	bin = buildCLI(t)
	dir := shortTempDir(t)

	backend := fakeChatBackend(t)
	sock = filepath.Join(dir, "mellomting.sock")
	cfgPath = filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("e2e-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: %s

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, landlockMode, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, cfgPath, sock, key
}

// syncBuffer is a goroutine-safe output sink for a child process:
// exec.Cmd's copy goroutines write into it while the test's Cleanup may
// read it, so the shared buffer must be locked (startServe). A plain
// bytes.Buffer races the copy goroutine even after Process.Wait returns.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startServe(t *testing.T, bin, cfgPath string) *exec.Cmd {
	cmd, _ := startServeWithLog(t, bin, cfgPath)
	return cmd
}

func startServeWithLog(t *testing.T, bin, cfgPath string) (*exec.Cmd, *syncBuffer) {
	t.Helper()
	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	var logB syncBuffer
	cmd.Stdout = &logB
	cmd.Stderr = &logB
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		t.Log(strings.TrimSpace(logB.String()))
	})
	return cmd, &logB
}

func waitReady(t *testing.T, sock string) *http.Client {
	t.Helper()
	client := unixHTTPClient(sock)
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	var lastCode, lastBody string
	for time.Now().Before(deadline) {
		// The daemon needs a moment to spawn and bind; dial errors
		// are retryable.
		req, err := http.NewRequest(http.MethodGet, "http://mellomting/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastCode, lastBody = strconv.Itoa(resp.StatusCode), string(b)
		if resp.StatusCode == 200 && string(b) == "ready" {
			return client
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon not ready: %s %q lastErr=%v", lastCode, lastBody, lastErr)
	return nil
}

func TestServeEndToEnd(t *testing.T) {
	bin, cfgPath, sock, key := serveFixture(t, "disabled")
	cmd := startServe(t, bin, cfgPath)
	client := waitReady(t, sock)

	t.Run("health", func(t *testing.T) {
		resp, body := getURL(t, client, "http://mellomting/healthz", "")
		if resp.StatusCode != 200 || body != "ok" {
			t.Fatalf("healthz: %d %q", resp.StatusCode, body)
		}
		if rid := resp.Header.Get("X-Request-ID"); !strings.HasPrefix(rid, "req_") {
			t.Fatalf("request id = %q", rid)
		}
	})

	t.Run("socket mode", func(t *testing.T) {
		st, err := os.Lstat(sock)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o660 {
			t.Fatalf("socket mode = %v", st.Mode().Perm())
		}
	})

	t.Run("auth", func(t *testing.T) {
		resp, body := getURL(t, client, "http://mellomting/v1/models", "")
		if resp.StatusCode != 401 {
			t.Fatalf("unauthenticated: %d %s", resp.StatusCode, body)
		}
		resp, body = postJSON(t, client, "http://mellomting/v1/chat/completions",
			"mtk_BADBAD_nope_nope_nope_nope_nope", `{"model":"qwen-coder"}`)
		if resp.StatusCode != 401 {
			t.Fatalf("bad key: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("models", func(t *testing.T) {
		resp, body := getURL(t, client, "http://mellomting/v1/models", key)
		if resp.StatusCode != 200 {
			t.Fatalf("models: %d %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "qwen-coder") || !strings.Contains(body, "mellomting") {
			t.Fatalf("models body = %s", body)
		}
	})

	t.Run("chat completion", func(t *testing.T) {
		resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
			`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != 200 {
			t.Fatalf("chat: %d %s", resp.StatusCode, body)
		}
		// The model the frontend saw must be the public name; the
		// backend saw the upstream name (asserted indirectly: a 200
		// here means the rewrite resolved, and the backend 404s on
		// unknown models is not exercised — instead check no public
		// name leaked into a backend-visible failure path is covered
		// by the ACL test below).
		if !strings.Contains(body, `"id":"e2e-1"`) {
			t.Fatalf("chat body = %s", body)
		}
		// The fake backend echoes the model it received: the response
		// must carry the upstream name, proving the rewrite (PLAN §13).
		if !strings.Contains(body, `"model":"Qwen/Qwen3-Coder"`) {
			t.Fatalf("rewrite missing; body = %s", body)
		}
	})

	t.Run("chat stream", func(t *testing.T) {
		resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
			`{"model":"qwen-coder","stream":true,"messages":[]}`)
		if resp.StatusCode != 200 {
			t.Fatalf("stream: %d %s", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("stream content-type = %q", ct)
		}
		for _, want := range []string{`"content":"He"`, "event: mellomting.test", "[DONE]"} {
			if !strings.Contains(body, want) {
				t.Fatalf("stream body missing %q: %s", want, body)
			}
		}
	})

	t.Run("model acl", func(t *testing.T) {
		resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
			`{"model":"not-allowed"}`)
		if resp.StatusCode != 404 || !strings.Contains(body, "model_not_found_or_not_allowed") {
			t.Fatalf("acl: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("no catch-all", func(t *testing.T) {
		resp, body := getURL(t, client, "http://mellomting/metrics", key)
		if resp.StatusCode != 404 {
			t.Fatalf("/metrics: %d %s", resp.StatusCode, body)
		}
		resp, body = postJSON(t, client, "http://mellomting/v1/load_lora_adapter", key, `{}`)
		if resp.StatusCode != 404 {
			t.Fatalf("/v1/load_lora_adapter: %d %s", resp.StatusCode, body)
		}
	})

	// Graceful shutdown: SIGTERM must exit 0 within the grace period.
	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	waitStart := time.Now()
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 0 {
			t.Fatalf("serve exit = %d", ee.ExitCode())
		}
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
		if elapsed := time.Since(waitStart); elapsed > 5*time.Second {
			t.Fatalf("shutdown took %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
}

func isExit(err error, code int) bool {
	ee, ok := err.(*exec.ExitError)
	return ok && ee.ExitCode() == code
}

// TestSafeUnixListen covers PLAN §8.3 socket-safety rules.
func TestSafeUnixListen(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")

	// Symlink must be refused.
	target := filepath.Join(dir, "target.sock")
	if err := os.Symlink(target, sock); err != nil {
		t.Fatal(err)
	}
	if _, err := safeUnixListen(configListenUnix(sock, "0660")); err == nil {
		t.Fatal("symlink accepted")
	}

	// A non-socket regular file must be refused.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := safeUnixListen(configListenUnix(sock, "0660")); err == nil {
		t.Fatal("regular file accepted")
	}

	// Clean bind, mode applied, stale socket replaced.
	os.Remove(sock)
	ln1, err := safeUnixListen(configListenUnix(sock, "0660"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o660 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	// A live socket must not be silently stolen (T-L9): a second listen
	// on the same path while ln1 is alive must refuse.
	if _, err := safeUnixListen(configListenUnix(sock, "0660")); err == nil {
		t.Fatal("live socket stolen")
	}
	_ = ln1.Close()
	ln2, err := safeUnixListen(configListenUnix(sock, "0660"))
	if err != nil {
		t.Fatalf("stale socket replacement: %v", err)
	}
	_ = ln2.Close()
}

func TestMPTCPListenersDisabled(t *testing.T) {
	// PLAN §83: the listener constructors explicitly disable MPTCP. The
	// behavioural (socket-level) assertion lives in mptcp_linux_test.go;
	// this just confirms a TCP listener binds.
	ln, err := mptcpOffListen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
}

// TestServeSandboxRequiredFails verifies that landlock.mode=required is
// fail-closed when the sandbox cannot be enforced (non-Linux, no kernel
// support, or ABI below the configured minimum): startup must fail with
// exit 1 (PLAN §55, §57). On a kernel that can enforce the default
// minimum ABI, required mode succeeds instead; that path is covered by
// TestServeSandboxRequiredApplies.
func TestServeSandboxRequiredFails(t *testing.T) {
	report := landlock.Check()
	if report.Supported && report.KernelABI >= landlock.DefaultMinimumABI {
		t.Skip("landlock is available at the default minimum ABI: required mode enforces the policy instead of failing; see TestServeSandboxRequiredApplies")
	}
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)

	// The daemon reads the key store before the sandbox gate (PLAN
	// §17: secrets before Landlock), so the store must be well-formed.
	pepperPath := filepath.Join(dir, "p")
	if err := os.WriteFile(pepperPath, []byte("sandbox-test-pepper-16b+"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(filepath.Join(dir, "users.yaml"), func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "s",
			SecretHash: auth.FormatHashValue(auth.Hash([]byte("sandbox-test-pepper-16b+"), k)),
			Enabled:    true, Models: []string{"m"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: required

backends:
  local-a:
    base_url: %s
    upstream_model: Up/Model

models:
  m:
    type: generation
    strategy: single
    backends:
      - local-a
`, filepath.Join(dir, "s.sock"), filepath.Join(dir, "users.yaml"), filepath.Join(dir, "p"), backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	var outB, errB bytes.Buffer
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	runErr := cmd.Run()
	exitCode := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		t.Fatal(runErr)
	}
	if exitCode != 1 {
		t.Fatalf("exit = %d (want 1; required sandbox must fail closed)", exitCode)
	}
	combined := outB.String() + errB.String()
	if !strings.Contains(combined, "landlock") {
		t.Fatalf("no landlock mention in output: %q", combined)
	}
}

// configListenUnix builds a unix listen config for listener tests.
func configListenUnix(address, mode string) config.Listen {
	return config.Listen{Network: "unix", Address: address, Mode: mode}
}

// writeSelfSignedTLS writes a self-signed certificate (CN localhost,
// SAN localhost/127.0.0.1) and its key into dir (PLAN §67 tests).
func writeSelfSignedTLS(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	derKey, err := x509.MarshalPKCS8PrivateKey(key)
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
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	derCert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
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
	keyPath = filepath.Join(dir, "key.pem")
	keyOut, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: derKey}); err != nil {
		t.Fatal(err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// tlsHTTPClient returns an https client that trusts the test certificate.
func tlsHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// TestServeStaticTLS runs the daemon on a TCP listener wrapped with the
// configured static certificate (PLAN §67): /healthz answers over TLS,
// the ready log records tls=true, and a plaintext client is rejected.
func TestServeStaticTLS(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)

	// Grab a free loopback port.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	certPath, keyPath := writeSelfSignedTLS(t, dir)
	pepperPath := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(pepperPath, []byte("e2e-tls-pepper-long-enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	usersPath := filepath.Join(dir, "users.yaml")
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash([]byte("e2e-tls-pepper-long-enough"), key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: tcp
    address: %s
  tls:
    mode: files
    cert_file: %s
    key_file: %s

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, addr, certPath, keyPath, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := startServe(t, bin, cfgPath)

	// Wait for readiness over TLS.
	client := tlsHTTPClient()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + addr + "/readyz")
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, body := getURL(t, client, "https://"+addr+"/healthz", "")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz over TLS: %d %s", resp.StatusCode, body)
	}

	// An authenticated completion works end to end over TLS.
	resp2, body2 := postJSON(t, client, "https://"+addr+"/v1/chat/completions", key,
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != 200 {
		t.Fatalf("chat over TLS: %d %s", resp2.StatusCode, body2)
	}
	if !strings.Contains(body2, `"id":"e2e-1"`) {
		t.Fatalf("chat body = %s", body2)
	}

	// Plaintext against the TLS listener must not reach the app: the
	// standard library answers with a 400 ("client sent an HTTP request
	// to an HTTPS server") instead of serving anything.
	plainReq, err := http.NewRequest(http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	plainResp, err := (&http.Client{Timeout: 3 * time.Second}).Do(plainReq)
	if err != nil {
		return // rejected at the connection level
	}
	plainBody, _ := io.ReadAll(plainResp.Body)
	plainResp.Body.Close()
	if plainResp.StatusCode == 200 && strings.TrimSpace(string(plainBody)) == "ok" {
		t.Fatal("plaintext client was served by the TLS listener")
	}

	// Clean shutdown (PLAN §74).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
}

// TestServeTLSHandshakeErrorsStructured proves that net/http's own error
// output (here a failed TLS handshake) is routed through the structured
// slog pipeline as a JSON ERROR record, never to raw stderr (T-L10).
func TestServeTLSHandshakeErrorsStructured(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	certPath, keyPath := writeSelfSignedTLS(t, dir)
	pepperPath := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(pepperPath, []byte("e2e-tls-pepper-long-enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	usersPath := filepath.Join(dir, "users.yaml")
	if err := os.WriteFile(usersPath, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: tcp
    address: %s
  tls:
    mode: files
    cert_file: %s
    key_file: %s

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, addr, certPath, keyPath, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if cmd.Process != nil && !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	client := tlsHTTPClient()
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + addr + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("TLS daemon never became ready:\n%s", logData)
	}

	// A plaintext client against the TLS listener forces a failed TLS
	// handshake, which net/http reports through Server.ErrorLog.
	plainReq, err := http.NewRequest(http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := (&http.Client{Timeout: 3 * time.Second}).Do(plainReq)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Give the server a moment to write the handshake error record.
	time.Sleep(500 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	killed = true

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(logData), "\n")
	matched := false
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		if !json.Valid([]byte(ln)) {
			t.Fatalf("non-JSON log line escaped the pipeline (T-L10): %q", ln)
		}
		if strings.Contains(ln, "TLS handshake") && strings.Contains(ln, `"level":"ERROR"`) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("no structured TLS handshake ERROR record found:\n%s", logData)
	}
}

// TestServePlaintextLocalhostWarns is the T-Q11 regression test: a
// hostname listener (localhost) is treated like any other non-loopback
// listener (matching config validation), so when the operator opts in
// with allow_plaintext_non_loopback the §8.2 startup warning MUST fire.
func TestServePlaintextLocalhostWarns(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	pepperPath := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(pepperPath, []byte("e2e-localhost-pepper-16b"), 0o600); err != nil {
		t.Fatal(err)
	}
	usersPath := filepath.Join(dir, "users.yaml")
	if err := os.WriteFile(usersPath, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: tcp
    address: localhost:%s
  allow_plaintext_non_loopback: true

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, strings.Split(addr, ":")[1], usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	// Wait for readiness over plaintext localhost.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://localhost:" + strings.Split(addr, ":")[1] + "/readyz")
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	killed = true

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "plaintext non-loopback TCP listener is active (PLAN §8.2)") {
		t.Fatalf("§8.2 warning did not fire for localhost listener:\n%s", logData)
	}
}

// TestServeRejectsShelved verifies that serve fails closed when a shelved
// feature is configured (PLAN §68, §96): ACME TLS and qualifiers.
func TestServeRejectsShelved(t *testing.T) {
	bin := buildCLI(t)
	backend := fakeChatBackend(t)

	t.Run("acme_tls", func(t *testing.T) {
		dir := shortTempDir(t)
		cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: tcp
    address: 127.0.0.1:18080
  tls:
    mode: acme
    hostname: llm.example.net
    email: admin@example.net

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Up/Model

models:
  m:
    type: generation
    strategy: single
    backends:
      - local-a
`, filepath.Join(dir, "users.yaml"), filepath.Join(dir, "pepper"), backend.URL)
		if err := assertServeRefused(t, bin, dir, cfg, "shelved"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("qualifier", func(t *testing.T) {
		dir := shortTempDir(t)
		sock := filepath.Join(dir, "mellomting.sock")
		cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Up/Model

qualifiers:
  safety-audit:
    backend: local-a
    model: Up/Model
    failure_policy: allow
    input:
      mode: audit
    output:
      mode: disabled

models:
  m:
    type: generation
    strategy: single
    backends:
      - local-a
    qualifier: safety-audit
`, sock, filepath.Join(dir, "users.yaml"), filepath.Join(dir, "pepper"), backend.URL)
		if err := assertServeRefused(t, bin, dir, cfg, "shelved"); err != nil {
			t.Fatal(err)
		}
	})
}

// assertServeRefused writes cfg, runs serve, and asserts exit 1 with the
// want marker in the combined output.
func assertServeRefused(t *testing.T, bin, dir, cfg, want string) error {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	var outB, errB bytes.Buffer
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	runErr := cmd.Run()
	exitCode := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		return fmt.Errorf("serve run: %w", runErr)
	}
	combined := outB.String() + errB.String()
	if exitCode != 1 {
		return fmt.Errorf("exit = %d (want 1); output: %q", exitCode, combined)
	}
	if !strings.Contains(combined, want) {
		return fmt.Errorf("output lacks %q: %q", want, combined)
	}
	return nil
}

// sandboxTestConfig returns a minimal config for applySandbox tests.
func sandboxTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := shortTempDir(t)
	return &config.Config{
		Auth: config.Auth{
			UsersFile:  filepath.Join(dir, "users.yaml"),
			PepperFile: filepath.Join(dir, "pepper"),
		},
		Backends: map[string]config.Backend{
			"b1": {BaseURL: "http://127.0.0.1:8001", UpstreamModel: "m"},
		},
		Security: config.Security{
			Landlock: config.Landlock{Mode: landlock.ModeRequired, MinimumABI: landlock.DefaultMinimumABI},
		},
	}
}

// TestEnforceSandboxModes verifies the mode dispatch of applySandbox
// (PLAN §55) without applying a real policy: the minimum ABI is set to
// a value no kernel can reach, so the gate rejects required mode and
// accepts best-effort on every platform. The real enforcement path is
// covered by TestAllThreadsEnforced (Linux) and the e2e tests.
func TestEnforceSandboxModes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := sandboxTestConfig(t)
	cfg.Security.Landlock.Mode = landlock.ModeDisabled
	if err := applySandbox(cfg, log); err != nil {
		t.Fatalf("disabled: %v", err)
	}

	cfg.Security.Landlock.Mode = landlock.ModeBestEffort
	cfg.Security.Landlock.MinimumABI = 255 // unreachable: force the gate
	if err := applySandbox(cfg, log); err != nil {
		t.Fatalf("best-effort must continue without a sandbox, got %v", err)
	}

	cfg.Security.Landlock.Mode = landlock.ModeRequired
	if err := applySandbox(cfg, log); err == nil {
		t.Fatal("required mode must fail closed when the sandbox cannot be enforced")
	}
}

// T-M10: systemctl reload must reach the graceful SIGHUP users reload
// (PLAN §64), never a kill/restart.
func TestSystemdUnitExecReload(t *testing.T) {
	data, err := os.ReadFile("../../deploy/mellomting.service")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "ExecReload=") || !strings.Contains(s, "-HUP") {
		t.Fatal("deploy/mellomting.service must ship ExecReload=/bin/kill -HUP $MAINPID so systemctl reload takes the graceful SIGHUP path")
	}
}

// waitStatus polls until the request reaches the wanted status code,
// tolerating the small window between a SIGHUP delivery and the daemon's
// in-place store swap (PLAN §30).
func waitStatus(t *testing.T, client *http.Client, method, url, key, body string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastCode int
	var lastBody string
	for time.Now().Before(deadline) {
		var resp *http.Response
		var b string
		if method == http.MethodGet {
			resp, b = getURL(t, client, url, key)
		} else {
			resp, b = postJSON(t, client, url, key, body)
		}
		lastCode, lastBody = resp.StatusCode, b
		if resp.StatusCode == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s %s: got %d %q after deadline (want %d)", method, url, lastCode, lastBody, want)
}

// T-M10: SIGHUP must gracefully reload the users file. A disabled or
// revoked key takes effect on the next request without a process restart,
// unrelated keys keep working, a failed edit keeps the previous store
// (fail closed), and the process survives SIGHUP (it no longer kills the
// daemon).
func TestServeSIGHUPReload(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("sighup-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	newKey := func(name string) (raw, id string) {
		t.Helper()
		k, id, err := auth.Generate()
		if err != nil {
			t.Fatal(err)
		}
		err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
			uf.Keys = append(uf.Keys, auth.Key{
				ID: id, Name: name,
				SecretHash: auth.FormatHashValue(auth.Hash(pepper, k)),
				Enabled:    true, Models: []string{"qwen-coder"},
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return k, id
	}
	keyA, idA := newKey("a")
	keyB, idB := newKey("b")

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := startServe(t, bin, cfgPath)
	client := waitReady(t, sock)
	chatURL := "http://mellomting/v1/chat/completions"
	chatBody := `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`

	// Both keys work initially.
	waitStatus(t, client, http.MethodPost, chatURL, keyA, chatBody, 200)
	waitStatus(t, client, http.MethodPost, chatURL, keyB, chatBody, 200)

	// Disable key A via the CLI, then SIGHUP: key A is rejected on the
	// next request without a restart, key B is unaffected, the process
	// survives.
	if code, _, errOut := runCLI(t, bin, dir, "key", "disable", "-config", cfgPath, "-id", idA); code != 0 {
		t.Fatalf("key disable exit = %d stderr=%q", code, errOut)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, client, http.MethodPost, chatURL, keyA, chatBody, 401)
	waitStatus(t, client, http.MethodPost, chatURL, keyB, chatBody, 200)
	if _, body := postJSON(t, client, chatURL, keyB, chatBody); !strings.Contains(body, `"id":"e2e-1"`) {
		t.Fatalf("key B chat after reload = %s", body)
	}

	// Re-enable key A and reload again: the change applies again.
	if code, _, errOut := runCLI(t, bin, dir, "key", "enable", "-config", cfgPath, "-id", idA); code != 0 {
		t.Fatalf("key enable exit = %d stderr=%q", code, errOut)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, client, http.MethodPost, chatURL, keyA, chatBody, 200)

	// Revoke key B and reload: key B is rejected live, key A still works.
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfgPath, "-id", idB); code != 0 {
		t.Fatalf("key revoke B exit = %d stderr=%q", code, errOut)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, client, http.MethodPost, chatURL, keyB, chatBody, 401)
	waitStatus(t, client, http.MethodPost, chatURL, keyA, chatBody, 200)

	// Revoke the last key and reload: the daemon survives and fails
	// closed (every request is 401) until an operator adds a key again.
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfgPath, "-id", idA); code != 0 {
		t.Fatalf("key revoke last exit = %d stderr=%q", code, errOut)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, client, http.MethodPost, chatURL, keyA, chatBody, 401)
	if resp, body := getURL(t, client, "http://mellomting/readyz", ""); resp.StatusCode != 200 || body != "ready" {
		t.Fatalf("daemon not ready after revoking all keys: %d %q", resp.StatusCode, body)
	}

	// A failed edit (malformed file) leaves the previous store in effect:
	// fail closed, never a silent empty/widened store.
	if err := os.WriteFile(usersPath, []byte("version: 99\nkeys: not-a-list"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if resp, body := getURL(t, client, "http://mellomting/readyz", ""); resp.StatusCode != 200 || body != "ready" {
		t.Fatalf("daemon not ready after failed reload: %d %q", resp.StatusCode, body)
	}
	if resp, body := getURL(t, client, "http://mellomting/v1/models", keyA); resp.StatusCode != 401 {
		t.Fatalf("failed reload changed auth state: %d %q", resp.StatusCode, body)
	}

	// The process is still the same one: SIGTERM shuts it down cleanly.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait after reloads: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM following SIGHUP reloads")
	}
}

// T-M11: a second SIGTERM during a drain must force a prompt exit (not be
// swallowed) and still flush accounting, even when an active stream would
// otherwise hold the drain open until the grace deadline.
func TestServeSecondSignalForcesShutdown(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	release := make(chan struct{})
	backend := slowChatBackend(t, release)
	t.Cleanup(func() { close(release) })
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")
	accPath := filepath.Join(dir, "usage.jsonl")

	pepper := []byte("force-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

accounting:
  enabled: true
  path: %s

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, accPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := startServe(t, bin, cfgPath)
	client := waitReady(t, sock)

	// One completed request so there is an accounting record to flush.
	resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}

	// Keep a handler alive across shutdown: a streaming request whose
	// backend holds the response open on release.
	req, err := http.NewRequest(http.MethodPost, "http://mellomting/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	doneReq := make(chan error, 1)
	go func() { _, err := client.Do(req); doneReq <- err }()
	// Give the request time to reach the backend and start streaming.
	time.Sleep(300 * time.Millisecond)

	// First SIGTERM: the graceful drain starts but is held open by the
	// active stream.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	// Second SIGTERM: must force a prompt exit with an accounting flush,
	// not be swallowed.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("second SIGTERM did not force prompt exit; took %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after the second SIGTERM")
	}

	// Accounting was flushed on the forced path: the completed request's
	// record is in the log.
	data, err := os.ReadFile(accPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"model":"qwen-coder"`) {
		t.Fatalf("accounting not flushed on forced shutdown: %q", data)
	}
}

// T-L12: a clean shutdown must not log a spurious "listener close:
// use of closed network connection" WARN (Shutdown already closed the
// listener; closing it again is expected, not a warning).
func TestServeCleanShutdownNoListenerWarn(t *testing.T) {
	bin, cfgPath, sock, _ := serveFixture(t, "disabled")

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	dir := filepath.Dir(cfgPath)
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if cmd.Process != nil && !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	_ = waitReady(t, sock)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit cleanly after SIGTERM")
	}
	killed = true

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "listener close") ||
		strings.Contains(string(logData), "closed network connection") {
		t.Fatalf("clean shutdown logged a spurious listener-close warning:\n%s", logData)
	}
}

// FIX-05/M19: while A drains with a stalled in-flight stream, B starts on
// the same Unix socket path. A's serve() only returns after its grace
// period; the old deferred os.Remove then fired against whatever owned the
// path, unlinking B's fresh socket and taking B off the path while its
// process kept running. After the fix B's socket must survive A's drain.
func TestDrainingDoesNotUnlinkReplacementSocket(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	release := make(chan struct{})
	defer close(release)
	backend := slowChatBackend(t, release)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("e2e-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

shutdown:
  grace_period: 3s

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// startDaemon spawns serve with its own log file and a cleanup that
	// kills it if it is still running when the test ends.
	startDaemon := func(name string) (*exec.Cmd, *http.Client) {
		t.Helper()
		cmd := exec.Command(bin, "serve", "-config", cfgPath)
		logPath := filepath.Join(dir, name+".log")
		logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = logF
		cmd.Stderr = logF
		if err := cmd.Start(); err != nil {
			logF.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			logF.Close()
			if cmd.Process != nil && cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		})
		return cmd, waitReady(t, sock)
	}

	cmdA, clientA := startDaemon("a")

	// Stall an in-flight streaming request so A stays in the drain for
	// its full grace period: its listener (and therefore the socket path)
	// is closed at the start of the drain, but serve() only returns once
	// the grace expires.
	req, err := http.NewRequest(http.MethodPost, "http://mellomting/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	first := make(chan struct{})
	streamDone := make(chan error, 1)
	go func() {
		resp, err := clientA.Do(req)
		if err != nil {
			streamDone <- err
			return
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		// The first flushed chunk means the backend was reached and the
		// connection is tracked as active by the server, so the drain
		// actually waits for it.
		if _, err := br.ReadString('\n'); err == nil {
			close(first)
		}
		_, _ = io.Copy(io.Discard, br)
		streamDone <- nil
	}()
	// Hold until the in-flight stream is established, then begin the
	// drain: A stays in it for its full grace period because its listener
	// (and socket path) is closed at the start of the drain while serve()
	// only returns once the grace expires.
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("stream never established")
	}

	if err := cmdA.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	// A's listener is closed (socket unlinked) at the start of the drain;
	// wait for the path to disappear so B can bind it without tripping
	// the T-L9 liveness check.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Lstat(sock); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A's socket was not unlinked after SIGTERM")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// B starts while A is still draining (start-before-stop replacement).
	_, clientB := startDaemon("b")
	resp, body := getURL(t, clientB, "http://mellomting/readyz", "")
	if resp.StatusCode != 200 || body != "ready" {
		t.Fatalf("B not reachable right after start: %d %q", resp.StatusCode, body)
	}

	// Wait for A to fully exit (past its 3s grace), when the old deferred
	// os.Remove would have fired.
	done := make(chan error, 1)
	go func() { done <- cmdA.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("A exited with error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("A did not exit within 15s")
	}
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled stream did not finish after A exited")
	}

	// The replacement's socket file must still exist and B must still be
	// reachable on the path.
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("replacement socket was unlinked by the draining process: %v", err)
	}
	resp, body = getURL(t, clientB, "http://mellomting/readyz", "")
	if resp.StatusCode != 200 || body != "ready" {
		t.Fatalf("B unreachable after A's drain: %d %q", resp.StatusCode, body)
	}
}

// T-L11: a TCP listener's shutdown must never attempt an os.Remove of a
// host:port-named relative path in the working directory. Before the fix,
// a pre-existing file named like the listen address was wiped on
// shutdown; after the fix it survives.
func TestServeTCPUsesNoUnlink(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// A file that an unguarded os.Remove("host:port") would delete.
	decoy := filepath.Join(dir, addr)
	if err := os.WriteFile(decoy, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")
	pepper := []byte("unlink-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: tcp
    address: %s

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: M

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, addr, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	cmd.Dir = dir // CWD where the unguarded relative os.Remove would land
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if cmd.Process != nil && !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	// Wait until the TCP listener answers readyz.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("TCP daemon never became ready (see daemon.log)")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	killed = true

	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("TCP shutdown removed the host:port-named file (T-L11): %v", err)
	}
}

// T-L15: accepted connections are bounded by server.max_connections.
// Excess connections are closed at accept time, so a client that dials
// past the cap cannot complete a request on the over-cap connections.
func TestServeMaxConnectionsEnforced(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("conn-cap-pepper-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usersPath, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"
  max_connections: 2

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: M

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = startServe(t, bin, cfgPath)
	_ = waitReady(t, sock)

	const total = 6
	conns := make([]net.Conn, 0, total)
	for i := 0; i < total; i++ {
		c, err := net.DialTimeout("unix", sock, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	// Let the server process the accepts and close the excess before
	// probing which connections are still alive.
	time.Sleep(500 * time.Millisecond)

	const cap = 2
	success := 0
	for _, c := range conns {
		if _, err := c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: mellomting\r\nConnection: close\r\n\r\n")); err != nil {
			continue
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 128)
		n, err := c.Read(buf)
		if err != nil {
			continue
		}
		if strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
			success++
		}
	}
	if success > cap {
		t.Fatalf("accepted %d connections, cap is %d (T-L15)", success, cap)
	}
	if success == 0 {
		t.Fatal("no connection succeeded; cap probe is broken")
	}
}

// TestServeSandboxRequiredApplies runs the daemon end to end with
// landlock.mode=required on a kernel that can enforce it (PLAN §55,
// §57): the daemon must begin accepting requests only after the policy
// has been applied to all threads, and it must operate normally
// afterwards (the policy allows the backend port).
func TestServeSandboxRequiredApplies(t *testing.T) {
	report := landlock.Check()
	if !report.Supported || report.KernelABI < landlock.DefaultMinimumABI {
		if os.Getenv("MELLOMTING_LANDLOCK_STRICT") != "" {
			t.Fatalf("strict landlock run: required-mode enforcement cannot be tested here (supported=%v kernel_abi=%d)", report.Supported, report.KernelABI)
		}
		t.Skipf("landlock unavailable or kernel ABI %d < default minimum %d; required-mode enforcement cannot be tested here", report.KernelABI, landlock.DefaultMinimumABI)
	}

	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("sandbox-required-pepper-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "sandbox",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: required

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd, logB := startServeWithLog(t, bin, cfgPath)
	client := waitReady(t, sock) // ready only after the sandbox is applied (PLAN §57 step 19)

	resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("chat under sandbox: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"id":"e2e-1"`) {
		t.Fatalf("chat body = %s", body)
	}

	// Reaching this point also proves the PLAN §57 ordering: a
	// required-mode daemon sets ready only after the sandbox has been
	// applied to all threads, and it served a request under the
	// confines of the policy.

	// N2 (FIX_REVIEW_2026-08-22): under landlock.mode: required the
	// users file is pinned to its startup inode, so a key rotation that
	// atomically renames the file cannot take effect on SIGHUP. Rewrite
	// the file with the key disabled (the same atomic rename `key
	// revoke` performs) and reload: the daemon must fail closed —
	// keeping the previous store, so the key still works — and must log
	// the ERROR naming the sandbox and the restart requirement instead
	// of silently no-opping.
	if err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		for i := range uf.Keys {
			if uf.Keys[i].ID == id {
				uf.Keys[i].Enabled = false
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logB.String(), "users reload failed; keeping previous store (under landlock.mode: required") {
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not log the sandbox-restart ERROR after SIGHUP; log=%s", logB.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The previous (enabled) store must still be in effect: the key
	// keeps working until a restart (PLAN §30 fail closed).
	resp, body = postJSON(t, client, "http://mellomting/v1/chat/completions", key,
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("key must keep working after failed sandboxed reload (fail closed): %d %s", resp.StatusCode, body)
	}

	err = cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && !isExit(err, 0) {
			t.Fatalf("serve wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
}

// TestServeAccountingDisabledStillEnforcesQuota proves that token quotas
// remain enforced when accounting.enabled: false (T-M5). Accounting off
// must never silently void a per-key token budget: the in-memory quota
// tracker runs regardless of JSONL persistence. A request whose
// reservation (the model cap) exceeds the key's hourly budget must be
// rejected 429 token_quota_exceeded, and the daemon must warn at startup
// that quotas are in-memory only.
func TestServeAccountingDisabledStillEnforcesQuota(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("quota-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
			Limits: auth.KeyLimits{TokensPerHour: 50},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

accounting:
  enabled: false

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    policy:
      max_output_tokens: 100
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	// Capture daemon logs to a file (not an in-memory buffer): the child
	// inherits the fd, so writes are complete once the process exits and
	// there is no parent-side io.Copy goroutine to race with.
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if cmd.Process != nil && !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	client := waitReady(t, sock)

	// The request's reservation is the injected 100-token cap, which
	// exceeds the key's 50-token hourly quota. It must be rejected even
	// though accounting (JSONL persistence) is disabled.
	resp, body := postJSON(t, client, "http://mellomting/v1/chat/completions", key,
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 || !strings.Contains(body, "token_quota_exceeded") {
		t.Fatalf("quota not enforced with accounting disabled: %d %s", resp.StatusCode, body)
	}

	// The daemon must also warn at startup that quotas are in-memory
	// only, so an operator who disabled accounting sees the consequence.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	killed = true
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "accounting disabled; token quotas are enforced in-memory only") {
		t.Fatalf("startup warning missing: %q", string(logData))
	}
}

// FIX-04/N10: with accounting disabled, ensure_stream_usage and
// unknown_usage_reservation are only meaningful when a per-key token quota
// is in effect. If no key carries a quota, serve must fail closed at
// startup instead of accepting a silent no-op (T-M12).
func TestServeAccountingOffQuotaSettingsRequireQuota(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("pepper-requires-16-bytes!!")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled

accounting:
  enabled: false
  ensure_stream_usage: true
  unknown_usage_reservation: 500

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    policy:
      max_output_tokens: 100
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Startup rejection must be quick and exit non-zero.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve started despite quota settings with no key quota configured")
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("serve did not exit within 30s; quota settings accepted as a silent no-op")
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "require a per-key token quota") {
		t.Fatalf("startup rejection message missing: %q", string(logData))
	}
}

// TestServeBackendNetworkAnyWarns proves that backend_network.mode: any
// is accepted (config remains valid) but emits the PLAN §16.3 startup
// warning, matching the other security-weakening warnings. A config
// that opts into unrestricted outbound egress must be surfaced to the
// operator, never a silent no-op (T-M12).
func TestServeBackendNetworkAnyWarns(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	backend := fakeChatBackend(t)
	sock := filepath.Join(dir, "mellomting.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("any-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "e2e",
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true, Models: []string{"qwen-coder"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: 1

server:
  listen:
    network: unix
    address: %s
    mode: "0660"

auth:
  users_file: %s
  pepper_file: %s

security:
  backend_network:
    mode: any
  landlock:
    mode: disabled

backends:
  local-a:
    base_url: %s
    upstream_model: Qwen/Qwen3-Coder

models:
  qwen-coder:
    type: generation
    strategy: single
    backends:
      - local-a
`, sock, usersPath, pepperPath, backend.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	logPath := filepath.Join(dir, "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if cmd.Process != nil && !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	client := waitReady(t, sock)
	if _, body := getURL(t, client, "http://mellomting/v1/models", key); !strings.Contains(body, "qwen-coder") {
		t.Fatalf("models body = %s", body)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	killed = true
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "backend_network.mode is") {
		t.Fatalf("mode:any startup warning missing: %q", string(logData))
	}
}

// T-T9: the server's MaxHeaderBytes bound is wired into newHTTPServer and
// must yield a 431 (never a proxy attempt) when a client exceeds it. A
// plain /healthz on the same server proves the bound is not rejecting
// normal requests.
func TestServeMaxHeaderBytesRejected(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Server: config.Server{
			MaxHeaderBytes:    1024,
			MaxConnections:    16,
			ReadHeaderTimeout: config.Duration(5 * time.Second),
			ReadBodyTimeout:   config.Duration(5 * time.Second),
			IdleTimeout:       config.Duration(30 * time.Second),
		},
	}
	api := httpapi.New(cfg, log, nil, nil, nil)
	srv := newHTTPServer(cfg, api, log)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})

	client := &http.Client{Timeout: 5 * time.Second}

	// A normal request is served by the real handler (healthz needs no key).
	resp, body := getURL(t, client, "http://"+ln.Addr().String()+"/healthz", "")
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}

	// An oversized header is rejected by net/http with 431 before any
	// handler runs. The enforced bound is MaxHeaderBytes plus net/http's
	// 4096-byte bufio slop, so the header must exceed both.
	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Big", strings.Repeat("a", 20000))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("oversized header request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized header: status = %d (want 431)", resp.StatusCode)
	}
}
