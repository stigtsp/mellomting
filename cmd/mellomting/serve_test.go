package main

import (
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
	"syscall"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
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

// serveFixture wires a temp config + users/pepper + fake backend.
// landlockMode is the security.landlock.mode of the written config.
func serveFixture(t *testing.T, landlockMode string) (bin, cfgPath, sock string, key string) {
	t.Helper()
	bin = buildCLI(t)
	dir := t.TempDir()

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

func startServe(t *testing.T, bin, cfgPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	var logB bytes.Buffer
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
	return cmd
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
	dir := t.TempDir()
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
	// PLAN §83: assert the constructors explicitly disable MPTCP.
	data, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `SetMultipathTCP(false)`) < 2 {
		t.Fatal("serve.go must disable MPTCP on both the TCP and unix listener constructors")
	}
	// Functional: a TCP listener binds.
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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

// TestServeRejectsShelved verifies that serve fails closed when a shelved
// feature is configured (PLAN §68, §96): ACME TLS and qualifiers.
func TestServeRejectsShelved(t *testing.T) {
	bin := buildCLI(t)
	backend := fakeChatBackend(t)

	t.Run("acme_tls", func(t *testing.T) {
		dir := t.TempDir()
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
		dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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

// T-L11: a TCP listener's shutdown must never attempt an os.Remove of a
// host:port-named relative path in the working directory. Before the fix,
// a pre-existing file named like the listen address was wiped on
// shutdown; after the fix it survives.
func TestServeTCPUsesNoUnlink(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
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

// TestServeSandboxRequiredApplies runs the daemon end to end with
// landlock.mode=required on a kernel that can enforce it (PLAN §55,
// §57): the daemon must begin accepting requests only after the policy
// has been applied to all threads, and it must operate normally
// afterwards (the policy allows the backend port).
func TestServeSandboxRequiredApplies(t *testing.T) {
	report := landlock.Check()
	if !report.Supported || report.KernelABI < landlock.DefaultMinimumABI {
		t.Skipf("landlock unavailable or kernel ABI %d < default minimum %d; required-mode enforcement cannot be tested here", report.KernelABI, landlock.DefaultMinimumABI)
	}

	bin, cfgPath, sock, key := serveFixture(t, "required")
	cmd := startServe(t, bin, cfgPath)
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
	err := cmd.Process.Signal(syscall.SIGTERM)
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
	dir := t.TempDir()
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

// TestServeBackendNetworkAnyWarns proves that backend_network.mode: any
// is accepted (config remains valid) but emits the PLAN §16.3 startup
// warning, matching the other security-weakening warnings. A config
// that opts into unrestricted outbound egress must be surfaced to the
// operator, never a silent no-op (T-M12).
func TestServeBackendNetworkAnyWarns(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
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
