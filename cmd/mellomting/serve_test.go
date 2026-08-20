package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// serveFixture wires a temp config + users/pepper + fake backend.
func serveFixture(t *testing.T) (bin, cfgPath, sock string, key string) {
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
	bin, cfgPath, sock, key := serveFixture(t)
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

func TestServeSandboxRequiredFails(t *testing.T) {
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
