package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var keyRe = regexp.MustCompile(`mtk_[A-Z2-9]{8}_[A-Z2-9]{52}`)

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mellomting")
	if err := exec.Command("go", "build", "-o", bin, ".").Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(bin) })
	return bin
}

func keyCLIFixture(t *testing.T) (bin, dir string) {
	t.Helper()
	bin = buildCLI(t)
	dir = t.TempDir()

	cfg := `version: 1

server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"

auth:
  users_file: ` + dir + `/users.yaml
  pepper_file: ` + dir + `/auth.pepper

servers:
  qwen-a:
    url: http://127.0.0.1:8001

models:
  qwen-coder:
    type: generation
    strategy: single
    upstream_model: Qwen/Qwen3-Coder-Next
    servers:
      - qwen-a
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.pepper"),
		[]byte("test-pepper-long-enough-16b+"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func runCLI(t *testing.T, bin, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var outB, errB strings.Builder
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return code, outB.String(), errB.String()
}

func TestKeyLifecycle(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	// Create a key.
	code, out, _ := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "tester", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	m := keyRe.FindString(out)
	if m == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	key := m
	// The raw key must be printed exactly once.
	if n := strings.Count(out, key); n != 1 {
		t.Fatalf("key printed %d times", n)
	}
	id := key[len("mtk_") : len("mtk_")+8]

	// List shows it enabled.
	code, out, _ = runCLI(t, bin, dir, "key", "list", "-config", cfg)
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "enabled") {
		t.Fatalf("list exit=%d out=%q", code, out)
	}

	// Disable / re-enable.
	if code, _, _ := runCLI(t, bin, dir, "key", "disable", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("disable exit = %d", code)
	}
	_, out, _ = runCLI(t, bin, dir, "key", "list", "-config", cfg)
	if !strings.Contains(out, "disabled") {
		t.Fatalf("not disabled: %q", out)
	}
	if code, _, _ := runCLI(t, bin, dir, "key", "enable", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("enable exit = %d", code)
	}

	// Second key with wildcard, then revoke it.
	code, out, _ = runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "wild", "-models", "*",
		"-expires", "2030-01-01T00:00:00Z")
	if code != 0 {
		t.Fatalf("create wildcard exit = %d", code)
	}
	key2 := keyRe.FindString(out)
	if key2 == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id2 := key2[4:12]
	if code, _, _ := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", id2); code != 0 {
		t.Fatalf("revoke exit = %d", code)
	}
	_, out, _ = runCLI(t, bin, dir, "key", "list", "-config", cfg)
	if strings.Contains(out, id2) {
		t.Fatalf("revoked key still listed: %q", out)
	}
	if !strings.Contains(out, id) {
		t.Fatalf("surviving key missing: %q", out)
	}

	// The created key must actually authenticate: verify hash chain by
	// re-creating a store (done in the auth package; here we only check
	// the users file is well-formed and owned 0600).
	ui, err := os.Stat(filepath.Join(dir, "users.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := ui.Mode().Perm(); perm != 0o600 {
		t.Fatalf("users.yaml mode = %v, want 0600", perm)
	}

	// Error paths.
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", "NOPE"); code != 1 {
		t.Fatalf("revoke unknown exit = %d stderr=%q", code, errOut)
	}
	if code, _, _ := runCLI(t, bin, dir, "key", "create", "-config", cfg, "-models", "x"); code != 2 {
		t.Fatalf("create without name exit = %d (want 2)", code)
	}
	if code, _, _ := runCLI(t, bin, dir, "key", "bogus"); code != 2 {
		t.Fatalf("unknown subcommand exit = %d (want 2)", code)
	}
}

// T-L4: key create --models naming a model absent from the config still
// succeeds (an ACL may legitimately name a model about to be added), but
// prints a warning referencing the missing model.
func TestKeyCreateUnknownModelWarns(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, _, errOut := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "ahead", "-models", "does-not-exist")
	if code != 0 {
		t.Fatalf("create with unknown model exit = %d (want 0)", code)
	}
	if !strings.Contains(errOut, "does-not-exist") || !strings.Contains(errOut, "warning") {
		t.Fatalf("no warning for unknown model: stderr=%q", errOut)
	}

	// Control: a known model produces no unknown-model warning (the
	// landlock reload warning may still appear: the fixture config
	// defaults to landlock.mode: required).
	code, _, errOut = runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "known", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create with known model exit = %d", code)
	}
	if strings.Contains(errOut, "is not present in the configuration") {
		t.Fatalf("unexpected unknown-model warning: stderr=%q", errOut)
	}
}

// T-M10: revoking the last key must succeed so a single-key deployment can
// revoke through the CLI; the resulting empty users file stays valid and
// key list reports "no keys".
func TestKeyRevokeLastKey(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, out, _ := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "solo", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	key := keyRe.FindString(out)
	if key == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id := key[4:12]

	// Revoking the only key must not fail with "at least one key".
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("revoke last key exit = %d stderr=%q", code, errOut)
	}
	if code, out, _ := runCLI(t, bin, dir, "key", "list", "-config", cfg); code != 0 || !strings.Contains(out, "no keys") {
		t.Fatalf("list after revoking last key: exit=%d out=%q", code, out)
	}
}

// FIX-02 (eval residual): mutating a key under landlock.mode: required
// must warn that a live server applies the change only after a restart —
// under that mode a SIGHUP reload of the change is denied by the
// sandbox (the users file is pinned to its startup inode), so the
// warning must not recommend one. A silent success would be a false
// sense of security. Other modes stay silent.
func TestKeyRevokeWarnsLandlockRequiredReload(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	// Default fixture config: landlock.mode defaults to required, so the
	// reload warning must appear on a successful create.
	code, out, errOut := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "warn", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	if !strings.Contains(errOut, "landlock") || !strings.Contains(errOut, "restart") {
		t.Fatalf("no reload warning on create for landlock.mode=required: stderr=%q", errOut)
	}
	if strings.Contains(errOut, "SIGHUP") {
		t.Fatalf("create warning must not recommend SIGHUP (denied by the sandbox under mode=required): stderr=%q", errOut)
	}
	id := keyRe.FindString(out)
	if id == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id = id[4:12]

	// The same warning must appear on a successful revoke.
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("revoke exit = %d stderr=%q", code, errOut)
	} else if !strings.Contains(errOut, "landlock") || !strings.Contains(errOut, "restart") {
		t.Fatalf("no reload warning for landlock.mode=required: stderr=%q", errOut)
	} else if strings.Contains(errOut, "SIGHUP") {
		t.Fatalf("revoke warning must not recommend SIGHUP (denied by the sandbox under mode=required): stderr=%q", errOut)
	}

	// Control: best-effort mode must stay silent. Write a second config
	// with an explicit security section and repeat.
	base, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	beCfg := string(base) + "\nsecurity:\n  landlock:\n    mode: best-effort\n"
	bePath := filepath.Join(dir, "config-best-effort.yaml")
	if err := os.WriteFile(bePath, []byte(beCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runCLI(t, bin, dir,
		"key", "create", "-config", bePath, "-name", "warn2", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create (best-effort) exit = %d", code)
	} else if strings.Contains(errOut, "landlock") {
		t.Fatalf("unexpected reload warning on create for best-effort: stderr=%q", errOut)
	}
	id = keyRe.FindString(out)
	if id == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id = id[4:12]
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", bePath, "-id", id); code != 0 {
		t.Fatalf("revoke (best-effort) exit = %d stderr=%q", code, errOut)
	} else if strings.Contains(errOut, "landlock") {
		t.Fatalf("unexpected reload warning for best-effort: stderr=%q", errOut)
	}
}

// T-X8: key create records per-key limits flags in the users file, and
// rejects negative limit values.
func TestKeyCreateLimits(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, out, _ := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "limited",
		"-models", "qwen-coder",
		"-concurrent-requests", "2", "-requests-per-second", "5", "-burst", "10")
	if code != 0 {
		t.Fatalf("create with limits exit = %d", code)
	}
	if !strings.Contains(out, "concurrent_requests: 2") || !strings.Contains(out, "requests_per_second: 5") {
		t.Fatalf("limits not echoed in output: %q", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, "users.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "concurrent_requests: 2") ||
		!strings.Contains(string(data), "requests_per_second: 5") ||
		!strings.Contains(string(data), "burst: 10") {
		t.Fatalf("limits block missing from users file:\n%s", data)
	}

	// Negative limits fail closed at the CLI.
	for _, args := range [][]string{
		{"-concurrent-requests", "-1"},
		{"-requests-per-second", "-1"},
		{"-burst", "-1"},
	} {
		code, _, _ := runCLI(t, bin, dir, append([]string{
			"key", "create", "-config", cfg, "-name", "neg", "-models", "x",
		}, args...)...)
		if code != 2 {
			t.Fatalf("negative limit %v exit = %d (want 2)", args, code)
		}
	}
}
