package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/systemd"
	"mellomting/internal/testsupport"
)

// modelsBackend is an inference server advertising model "m".
func modelsBackend(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// One binary, two roles: an edge daemon accepting agents, and a daemon
// that only attaches a local server to it. A client reaches the server
// through the edge, and stopping the attaching daemon detaches it.
func TestServeAttachOnlyEndToEnd(t *testing.T) {
	bin := buildCLI(t)
	dir := shortTempDir(t)
	be := modelsBackend(t)
	port := freePort(t)

	pepper := []byte("attach-pepper-long-enough-16b")
	usersPath := filepath.Join(dir, "users.yaml")
	pepperPath := filepath.Join(dir, "auth.pepper")
	if err := os.WriteFile(pepperPath, pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	agentKey, agentID, err := auth.Generate("lab")
	if err != nil {
		t.Fatal(err)
	}
	clientKey, clientID, err := auth.Generate("alice")
	if err != nil {
		t.Fatal(err)
	}
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys,
			auth.Key{ID: agentID, Name: "lab", SecretHash: auth.FormatHashValue(auth.Hash(pepper, agentKey)), Enabled: true, Agent: "lab"},
			auth.Key{ID: clientID, Name: "alice", SecretHash: auth.FormatHashValue(auth.Hash(pepper, clientKey)), Enabled: true, Models: []string{"lab/*"}},
		)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	edgeCfg := filepath.Join(dir, "edge.yaml")
	writeFile(t, edgeCfg, fmt.Sprintf(`version: 1
server:
  listen: {network: tcp, address: "127.0.0.1:%d"}
auth:
  users_file: %s
  pepper_file: %s
security:
  landlock: {mode: disabled}
  seatbelt: {mode: disabled}
agents:
  enabled: true
`, port, usersPath, pepperPath))
	startServe(t, bin, edgeCfg)

	keyPath := filepath.Join(dir, "agent.key")
	writeFile(t, keyPath, agentKey+"\n")
	attachCfg := filepath.Join(dir, "attach.yaml")
	writeFile(t, attachCfg, fmt.Sprintf(`version: 1
security:
  landlock: {mode: disabled}
  seatbelt: {mode: disabled}
attach:
  edge: http://127.0.0.1:%d
  key_file: %s
  servers: [%q]
`, port, keyPath, be.URL))
	attach, logB := startServeWithLog(t, bin, attachCfg)

	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	body := `{"model":"lab/m","messages":[]}`
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+clientKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lab/m never served through the edge (last err %v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(logB.String(), `"role":"attach"`) && !strings.Contains(logB.String(), "role=attach") {
		t.Errorf("attach-only daemon did not report its role:\n%s", logB.String())
	}

	if err := attach.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- attach.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach-only daemon exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach-only daemon did not stop on SIGTERM")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// init --edge --dry-run renders an attach-only configuration that serve
// accepts, with the CA and key copies beside it.
func TestInitEdgeDryRun(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, ca, "not checked in a dry run")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	code, out, errOut := captureOutput(t, func() int {
		return initCmd([]string{"--edge", "https://edge.example.org", "--ca-file", ca, "--config", cfgPath, "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	cfg, err := config.Parse([]byte(out))
	if err != nil {
		t.Fatalf("rendered configuration: %v\n%s", err, out)
	}
	a := cfg.Attach
	dir := filepath.Dir(cfgPath)
	if cfg.ServesClients() || a == nil || a.KeyFile != filepath.Join(dir, "agent.key") ||
		a.CAFile != filepath.Join(dir, "edge-ca.pem") || len(a.Servers) != 1 || a.Servers[0] != defaultAttachServer {
		t.Fatalf("rendered attach section: %+v", a)
	}
}

func TestInitEdgeFlagConflicts(t *testing.T) {
	for _, args := range [][]string{
		{"--edge", "https://e.example.org", "--listen", "127.0.0.1:9000", "--dry-run"},
		{"--server", "http://127.0.0.1:8000", "--ca-file", "/x", "--dry-run"},
		{"--edge", "http://edge.example.org", "--dry-run"},
	} {
		if code, _, _ := captureOutput(t, func() int { return initCmd(args) }); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

// The attach set goes through the same transaction as init's own: the
// configuration last, and nothing left behind when a step fails.
func TestCommitAttachFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("init commit is Linux-only")
	}
	set := func(dir string) []commitFile {
		return []commitFile{
			{path: filepath.Join(dir, "agent.key"), data: []byte("key\n")},
			{path: filepath.Join(dir, "backend.key"), data: []byte("token\n")},
			{path: filepath.Join(dir, "edge-ca.pem"), data: []byte("pem")},
			{path: filepath.Join(dir, "config.yaml"), data: []byte("version: 1\n")},
		}
	}
	dir := newCommitDir(t)
	ops := &scriptedCommitOps{inner: defaultCommitOps()}
	if err := commitFiles(set(dir), ops); err != nil {
		t.Fatal(err)
	}
	last := -1
	for i, c := range ops.calls {
		if strings.HasPrefix(c, "linkInDir:") {
			last = i
		}
	}
	if last < 0 || ops.calls[last] != "linkInDir:config.yaml" {
		t.Fatalf("config not published last: %v", ops.calls)
	}
	for _, f := range set(dir) {
		if b, err := os.ReadFile(f.path); err != nil || string(b) != string(f.data) {
			t.Fatalf("%s = %q, %v", f.path, b, err)
		}
	}

	// init --edge passes no ops: the production ones are the default.
	if err := commitFiles(set(newCommitDir(t)), nil); err != nil {
		t.Fatalf("default ops: %v", err)
	}

	dir = newCommitDir(t)
	ops = &scriptedCommitOps{inner: defaultCommitOps(), failOnce: map[string]bool{"linkInDir:config.yaml": true}}
	if err := commitFiles(set(dir), ops); err == nil {
		t.Fatal("injected failure not reported")
	}
	requireDirEmpty(t, dir)
}

// Only the untouched install scaffold is moved aside, and a failed
// commit puts it back.
func TestSetScaffoldAside(t *testing.T) {
	dir := t.TempDir()
	scaffold := filepath.Join(dir, "config.yaml")
	writeFile(t, scaffold, string(systemd.ConfigTemplate()))
	restore, err := setScaffoldAside(scaffold)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(scaffold); !os.IsNotExist(err) {
		t.Fatalf("scaffold still in the way: %v", err)
	}
	restore(false)
	if b, err := os.ReadFile(scaffold); err != nil || string(b) != string(systemd.ConfigTemplate()) {
		t.Fatalf("scaffold not restored: %v", err)
	}
	restore, _ = setScaffoldAside(scaffold)
	restore(true)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("committed: %d entries left, want none", len(entries))
	}

	writeFile(t, scaffold, string(systemd.ConfigTemplate())+"# edited\n")
	restore, _ = setScaffoldAside(scaffold)
	restore(true)
	if _, err := os.Lstat(scaffold); err != nil {
		t.Fatalf("edited configuration moved: %v", err)
	}
}

func TestAttachOptionsCarryMetricsSwitch(t *testing.T) {
	off := false
	a := &config.Attach{Edge: "https://edge.example.org", KeyFile: "/k", Servers: []string{"http://127.0.0.1:8000"}, Metrics: &off}
	opts, err := attachOptionsWithKey(a, "key", testsupport.DiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if opts[0].Metrics {
		t.Fatal("attach.metrics: false not passed to the agent")
	}
	a.Metrics = nil
	if opts, _ = attachOptionsWithKey(a, "key", testsupport.DiscardLogger()); !opts[0].Metrics {
		t.Fatal("metrics not on by default")
	}
}
