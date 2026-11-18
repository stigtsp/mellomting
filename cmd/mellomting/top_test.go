package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/topview"
)

// `top` answers the question an operator asks while something is slow:
// what is this daemon doing right now. The whole path is exercised —
// a real daemon, a real request held open by a slow backend, the admin
// socket, and the rendered table.
func TestTopShowsAnInFlightRequest(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)

	// A backend that holds the request open until the test lets go, so
	// there is something in flight to observe.
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	be := newSlowBackend(t, started, release)

	sock := filepath.Join(dir, "m.sock")
	admin := filepath.Join(dir, "admin.sock")
	cfgPath := filepath.Join(dir, "config.yaml")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")

	pepper := []byte("top-pepper-long-enough-16b")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	key, id, err := auth.Generate("watcher")
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID: id, Name: "watcher",
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
  admin_socket: %s

auth:
  users_file: %s
  pepper_file: %s

security:
  landlock:
    mode: disabled
  seatbelt:
    mode: disabled

servers:
  local-a:
    url: %s

models:
  qwen-coder:
    type: generation
    strategy: single
    upstream_model: Qwen/Qwen3-Coder
    servers:
      - local-a
`, sock, admin, usersPath, pepperPath, be.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	startServe(t, bin, cfgPath)
	client := waitReady(t, sock)

	// Hold one request open, with a user agent to look for.
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://mellomting/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("User-Agent", "top-test/9.9")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the backend never received the request")
	}

	// The admin socket must not be world-readable: it shows every
	// caller's address, user agent and token use.
	info, err := os.Stat(admin)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("admin socket mode = %#o, want 0600", perm)
	}

	deadline := time.Now().Add(10 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		code, stdout, stderr := runCLI(t, bin, dir, "top", "--once", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("top exit = %d stderr=%q", code, stderr)
		}
		out = stdout
		if strings.Contains(out, "top-test/9.9") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range []string{"1 in flight", "watcher", "qwen-coder", "top-test/9.9", "waiting"} {
		if !strings.Contains(out, want) {
			t.Fatalf("top output missing %q:\n%s", want, out)
		}
	}
}

// A daemon with no admin socket publishes no view, and top says so
// rather than failing obscurely.
func TestTopWithoutAdminSocket(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")
	code, _, stderr := runCLI(t, bin, dir, "top", "--once", "--config", cfg)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "admin_socket") {
		t.Fatalf("stderr = %q, want it to name the setting", stderr)
	}
}

// newSlowBackend answers only once the test releases it, so a request
// stays in flight long enough to be observed.
func newSlowBackend(t *testing.T, started chan<- struct{}, release <-chan struct{}) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// --simulate needs no configuration or daemon, and --once on a pipe
// prints the plain view without escape sequences.
func TestTopSimulateOnce(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	code, out, stderr := runCLI(t, bin, dir, "top", "--simulate", "--once")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{"in flight", "ATTENTION", "coding_agents", "refused", "agent-lab-6", "REQUESTS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Fatal("escape sequence on a pipe")
	}
}

func TestTopRedactFlag(t *testing.T) {
	bin := buildCLI(t)
	code, out, _ := runCLI(t, bin, t.TempDir(), "top", "--simulate", "--once", "--redact")
	if code != 0 || !strings.Contains(out, "REDACTED") || strings.Contains(out, "192.0.2.") || strings.Contains(out, "198.51.100.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A daemon from before /stats answers 404: top says so and shows the
// table it can.
func TestTopOldDaemonFallback(t *testing.T) {
	old := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inflight" {
			w.Write([]byte(`{"requests":[{"request_id":"r1","key_id":"k","key_name":"old-key","remote":"192.0.2.9:1","endpoint":"/v1/chat/completions","phase":"waiting","age_ms":1500}],"agents":null}`))
			return
		}
		http.NotFound(w, r)
	})
	client := &http.Client{Transport: handlerTransport{old}}
	var out, errw strings.Builder
	code := topOnce(context.Background(), &out, &errw, newAdminSource(client), topview.Options{Redact: true})
	if code != 0 || !strings.Contains(errw.String(), "older") || !strings.Contains(out.String(), "old-key") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out.String(), errw.String())
	}
	if strings.Contains(out.String(), "192.0.2.9") {
		t.Fatal("legacy table ignores --redact")
	}
}

// handlerTransport serves requests from a handler in-process, standing
// in for the admin socket.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.h.ServeHTTP(w, r)
	return w.Result(), nil
}
