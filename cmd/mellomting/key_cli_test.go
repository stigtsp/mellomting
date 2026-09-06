package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

var keyRe = regexp.MustCompile(`sk-[a-z][a-z0-9_]{0,31}-[0-9a-f]{16}-[0-9a-f]{64}`)

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
	code, out, errOut := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "tester", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	m := keyRe.FindString(out)
	if m == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	key := m
	// D14: stdout is the raw key and a trailing newline only (script-safe),
	// so it appears exactly once and nowhere else, including stderr.
	if out != key+"\n" {
		t.Fatalf("stdout must be the raw key and newline only: %q", out)
	}
	if strings.Contains(errOut, key) {
		t.Fatalf("raw key leaked to stderr: %q", errOut)
	}
	id := strings.Split(key, "-")[2]

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
	id2 := strings.Split(key2, "-")[2]
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
	id := strings.Split(key, "-")[2]

	// Revoking the only key must not fail with "at least one key".
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("revoke last key exit = %d stderr=%q", code, errOut)
	}
	if code, out, _ := runCLI(t, bin, dir, "key", "list", "-config", cfg); code != 0 || !strings.Contains(out, "no keys") {
		t.Fatalf("list after revoking last key: exit=%d out=%q", code, out)
	}
}

// A key mutation prints one apply instruction, and it is the same in
// every sandbox mode. It used to depend on landlock.mode: under
// "required" the policy granted the users file by pathname, every
// mutation renamed a new inode over it, and the reload was denied — so
// the instruction said "restart" and was forbidden from mentioning
// SIGHUP. The policy now grants the directory, so a reload applies key
// rotation and revocation under every mode.
func TestKeyMutationApplyInstruction(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	// D14: create is script-safe — stdout is the raw key and a trailing
	// newline only; the apply instruction goes to stderr.
	code, out, errOut := runCLI(t, bin, dir,
		"key", "create", "-config", cfg, "-name", "warn", "-models", "qwen-coder")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	if key := keyRe.FindString(out); key == "" || out != key+"\n" {
		t.Fatalf("stdout must be the raw key and newline only: %q", out)
	}
	if !strings.Contains(errOut, "Reload Mellomting to apply it") {
		t.Fatalf("no apply instruction on create: stderr=%q", errOut)
	}
	if !strings.Contains(errOut, "systemctl reload") {
		t.Fatalf("the apply instruction must name the reload command: stderr=%q", errOut)
	}
	id := keyRe.FindString(out)
	if id == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id = strings.Split(id, "-")[2]

	// The same instruction appears on a successful revoke, and stays
	// concise (D16): no design rationale or mechanism inventory.
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", cfg, "-id", id); code != 0 {
		t.Fatalf("revoke exit = %d stderr=%q", code, errOut)
	} else if !strings.Contains(errOut, "Reload Mellomting to apply it") {
		t.Fatalf("no apply instruction on revoke: stderr=%q", errOut)
	} else {
		for _, banned := range []string{"inode", "pinned", "atomic", "rename", "MPTCP"} {
			if strings.Contains(errOut, banned) {
				t.Fatalf("revoke output contains banned detail %q: stderr=%q", banned, errOut)
			}
		}
	}

	// The instruction no longer varies with the sandbox mode: a
	// best-effort config gets the same one, and neither mentions
	// landlock.
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
	}
	if !strings.Contains(errOut, "Reload Mellomting to apply it") {
		t.Fatalf("apply instruction differs by sandbox mode: stderr=%q", errOut)
	}
	if strings.Contains(errOut, "landlock") {
		t.Fatalf("the apply instruction must not mention landlock: stderr=%q", errOut)
	}
	id = keyRe.FindString(out)
	if id == "" {
		t.Fatalf("no raw key printed: %q", out)
	}
	id = strings.Split(id, "-")[2]
	if code, _, errOut := runCLI(t, bin, dir, "key", "revoke", "-config", bePath, "-id", id); code != 0 {
		t.Fatalf("revoke (best-effort) exit = %d stderr=%q", code, errOut)
	} else if strings.Contains(errOut, "landlock") {
		t.Fatalf("the apply instruction must not mention landlock: stderr=%q", errOut)
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
	// D14: limits are not echoed on stdout (already supplied by the
	// operator); stdout stays script-safe — the raw key and newline only.
	if key := keyRe.FindString(out); key == "" || out != key+"\n" {
		t.Fatalf("stdout must be the raw key and newline only: %q", out)
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

// D14: inferSoleModel resolves --models when it is absent — exactly one
// configured model is inferred, zero or two-or-more fail (listing at most 20
// names plus the omitted count), and "*" is never inferred.
func TestInferSoleModel(t *testing.T) {
	t.Run("zero models", func(t *testing.T) {
		_, err := inferSoleModel(&config.Config{Models: map[string]config.Model{}})
		if err == nil || !strings.Contains(err.Error(), "no models") {
			t.Fatalf("err = %v, want no models", err)
		}
	})

	t.Run("single model inferred", func(t *testing.T) {
		got, err := inferSoleModel(&config.Config{Models: map[string]config.Model{"only": {}}})
		if err != nil || !reflect.DeepEqual(got, []string{"only"}) {
			t.Fatalf("got %v err %v", got, err)
		}
	})

	t.Run("multiple models fail and list sorted names", func(t *testing.T) {
		_, err := inferSoleModel(&config.Config{Models: map[string]config.Model{"bravo": {}, "alpha": {}}})
		if err == nil || !strings.Contains(err.Error(), "multiple models") {
			t.Fatalf("err = %v, want multiple models", err)
		}
		if !strings.Contains(err.Error(), "alpha, bravo") {
			t.Fatalf("err = %q, want sorted names", err)
		}
		if strings.Contains(err.Error(), "*") {
			t.Fatalf("err = %q must not suggest *", err)
		}
	})

	t.Run("more than 20 models are truncated", func(t *testing.T) {
		m := make(map[string]config.Model, 25)
		for i := range 25 {
			m[fmt.Sprintf("m%02d", i)] = config.Model{}
		}
		_, err := inferSoleModel(&config.Config{Models: m})
		if err == nil || !strings.Contains(err.Error(), "and 5 more") {
			t.Fatalf("err = %v, want omitted count", err)
		}
		if !strings.Contains(err.Error(), "config show-effective") {
			t.Fatalf("err = %q, want show-effective pointer", err)
		}
		if strings.Contains(err.Error(), "m24") {
			t.Fatalf("err = %q must omit the 25th model", err)
		}
	})
}

// D18: chooseKeyID retries a key-ID collision at most maxAttempts times and
// then fails without mutation; a generator (entropy) failure is propagated.
func TestChooseKeyID(t *testing.T) {
	t.Run("success on first draw", func(t *testing.T) {
		uf := &auth.UsersFile{Keys: []auth.Key{{ID: "0000000000000000"}}}
		key, id, err := chooseKeyID(func() (string, string, error) {
			return "sk-a-1111111111111111-000000000000000000000000000000000000000000000000000000", "1111111111111111", nil
		}, uf, 8)
		if err != nil || id != "1111111111111111" || !strings.HasPrefix(key, "sk-a-") {
			t.Fatalf("got key=%q id=%q err=%v", key, id, err)
		}
	})

	t.Run("bounded collision retry", func(t *testing.T) {
		uf := &auth.UsersFile{Keys: []auth.Key{{ID: "2222222222222222"}}}
		calls := 0
		_, _, err := chooseKeyID(func() (string, string, error) {
			calls++
			return "sk-a-2222222222222222-000000000000000000000000000000000000000000000000000000", "2222222222222222", nil
		}, uf, 8)
		if err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("err = %v, want collision", err)
		}
		if calls != 8 {
			t.Fatalf("generate called %d times, want exactly 8", calls)
		}
		if len(uf.Keys) != 1 {
			t.Fatalf("users file mutated: %d keys", len(uf.Keys))
		}
	})

	t.Run("entropy failure propagates", func(t *testing.T) {
		uf := &auth.UsersFile{}
		_, _, err := chooseKeyID(func() (string, string, error) {
			return "", "", fmt.Errorf("entropy")
		}, uf, 8)
		if err == nil || !strings.Contains(err.Error(), "entropy") {
			t.Fatalf("err = %v, want entropy", err)
		}
	})
}

// D18: multiple keys may share a username; the key ID distinguishes them and
// both authenticate.
func TestMultipleKeysShareUsername(t *testing.T) {
	pepper := []byte("multi-username-pepper-16b")
	k1, id1, err := auth.Generate("shared")
	if err != nil {
		t.Fatal(err)
	}
	k2, id2, err := auth.Generate("shared")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("two keys for one username got the same ID")
	}
	uf := &auth.UsersFile{Version: 1, Keys: []auth.Key{
		{ID: id1, Name: "shared", SecretHash: auth.FormatHashValue(auth.Hash(pepper, k1)), Enabled: true, Models: []string{"*"}},
		{ID: id2, Name: "shared", SecretHash: auth.FormatHashValue(auth.Hash(pepper, k2)), Enabled: true, Models: []string{"*"}},
	}}
	store, err := auth.NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{k1, k2} {
		if _, err := store.Lookup(raw); err != nil {
			t.Fatalf("Lookup(%q): %v", raw, err)
		}
	}
}

// D18: --name outside the username grammar is rejected before any entropy use
// or filesystem mutation (exit 2, no users file).
func TestKeyCreateRejectsBadUsername(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")
	// "_a" pins that the underscore is legal only after the first
	// character, which must still be a letter.
	for _, name := range []string{"Bad", "1abc", "_a", "a-b", strings.Repeat("a", 33), ""} {
		code, _, errOut := runCLI(t, bin, dir, "key", "create", "-config", cfg, "-name", name, "-models", "qwen-coder")
		if code != 2 {
			t.Fatalf("--name %q: exit = %d, want 2 (stderr=%q)", name, code, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "users.yaml")); !os.IsNotExist(err) {
		t.Fatal("users.yaml must not be created for an invalid --name")
	}
}

// D14: key create with no --models infers the single configured model.
func TestKeyCreateInfersSoleModel(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfg := filepath.Join(dir, "config.yaml")

	code, out, _ := runCLI(t, bin, dir, "key", "create", "-config", cfg, "-name", "inferred")
	if code != 0 {
		t.Fatalf("create (no --models) exit = %d", code)
	}
	if keyRe.FindString(out) == "" {
		t.Fatalf("no raw key printed: %q", out)
	}

	// The inferred key must be scoped to the sole configured model.
	code, out, _ = runCLI(t, bin, dir, "key", "list", "-config", cfg)
	if code != 0 || !strings.Contains(out, "qwen-coder") {
		t.Fatalf("list exit=%d out=%q, want qwen-coder", code, out)
	}
}

// D14: with two or more configured models, key create without --models fails
// (usage error) and lists the model names; it writes no key.
func TestKeyCreateRefusesToInferMultiple(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
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
  alpha-model:
    type: generation
    servers:
      - qwen-a
  bravo-model:
    type: generation
    servers:
      - qwen-a
`
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.pepper"),
		[]byte("test-pepper-long-enough-16b+"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, bin, dir, "key", "create", "-config", cfgPath, "-name", "multi")
	if code != 2 {
		t.Fatalf("create (multiple models, no --models) exit = %d (want 2)", code)
	}
	if !strings.Contains(errOut, "alpha-model") || !strings.Contains(errOut, "bravo-model") {
		t.Fatalf("stderr missing model names: %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "users.yaml")); !os.IsNotExist(err) {
		t.Fatalf("users.yaml must not be created on inference failure")
	}
}
