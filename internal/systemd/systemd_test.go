package systemd

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/landlock"
)

// TestSyncEmbeddedMatchesDeploy proves the embedded unit (rendered with the
// default prefix), the logrotate, and the config scaffold are byte-identical
// to the canonical operator-facing copies in deploy/, so the two sets can
// never drift.
func TestSyncEmbeddedMatchesDeploy(t *testing.T) {
	repo := filepath.Join("..", "..")

	unit, err := os.ReadFile(filepath.Join(repo, "deploy", "mellomting.service"))
	if err != nil {
		t.Fatalf("read deploy/mellomting.service: %v", err)
	}
	if got := RenderService("/usr/local/bin/mellomting"); got != string(unit) {
		t.Errorf("embedded unit (default render) differs from deploy/mellomting.service")
	}

	logrotate, err := os.ReadFile(filepath.Join(repo, "deploy", "mellomting.logrotate"))
	if err != nil {
		t.Fatalf("read deploy/mellomting.logrotate: %v", err)
	}
	if string(Logrotate()) != string(logrotate) {
		t.Errorf("embedded logrotate differs from deploy/mellomting.logrotate")
	}

	scaffold, err := os.ReadFile(filepath.Join(repo, "deploy", "mellomting-config.yaml.example"))
	if err != nil {
		t.Fatalf("read deploy/mellomting-config.yaml.example: %v", err)
	}
	if string(ConfigTemplate()) != string(scaffold) {
		t.Errorf("embedded config scaffold differs from deploy/mellomting-config.yaml.example")
	}
}

// TestRenderServiceSubstitutesBinaryPath checks that a custom prefix is
// rendered into ExecStart and the placeholder never survives.
func TestRenderServiceSubstitutesBinaryPath(t *testing.T) {
	const custom = "/opt/mellomting/bin/mellomting"
	got := RenderService(custom)
	if !strings.Contains(got, "ExecStart="+custom+" serve -config /etc/mellomting/config.yaml") {
		t.Errorf("custom binary path not rendered into ExecStart:\n%s", got)
	}
	if strings.Contains(got, binaryPathPlaceholder) {
		t.Errorf("placeholder %q leaked into rendered unit", binaryPathPlaceholder)
	}
}

// TestPreflightFailClosed drives preflight with every invalid combination
// and confirms it rejects each one before mutating anything.
func TestPreflightFailClosed(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	valid := &Provision{BinaryPath: bin}

	cases := []struct {
		name string
		p    *Provision
		env  preflightEnv
		want string // substring expected in the error
	}{
		{"non-linux host", valid, preflightEnv{"darwin", 0, true}, "Linux"},
		{"not root", valid, preflightEnv{"linux", 1000, true}, "root"},
		{"systemd inactive", valid, preflightEnv{"linux", 0, false}, "systemd"},
		{"control character in binary path", &Provision{BinaryPath: "/tmp/mellomting\nExecStart=malicious"}, preflightEnv{"linux", 0, true}, "control character"},
		{"relative binary path", &Provision{BinaryPath: "mellomting"}, preflightEnv{"linux", 0, true}, "not absolute"},
		{"missing binary", &Provision{BinaryPath: "/nonexistent/mellomting"}, preflightEnv{"linux", 0, true}, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.preflight(tc.env)
			if err == nil {
				t.Fatalf("preflight unexpectedly passed for %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("preflight error %q does not mention %q", err, tc.want)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		if err := valid.preflight(preflightEnv{"linux", 0, true}); err != nil {
			t.Errorf("valid preflight failed: %v", err)
		}
	})
}

// TestResolveBinaryNotFound proves the fixed-location resolver fails
// closed (no $PATH) and names the binary when none of its locations hold
// a regular file.
func TestResolveBinaryNotFound(t *testing.T) {
	got, err := resolveBinary("nosuchbinary", "/nonexistent/nosuchbinary-a", "/nonexistent/nosuchbinary-b")
	if err == nil {
		t.Fatalf("resolveBinary for a missing binary succeeded: %q", got)
	}
	if !strings.Contains(err.Error(), "nosuchbinary") {
		t.Fatalf("error does not name the binary: %v", err)
	}
}

// TestEnsureConfig covers the scaffold step of provisioning: it creates
// the scaffold with the requested mode when the config is absent, never
// rewrites an existing config, and refuses a non-regular file at the path.
func TestEnsureConfig(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()

	t.Run("creates scaffold", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		created, err := ensureConfig(path, 0o640, uid, gid)
		if err != nil || !created {
			t.Fatalf("ensureConfig = created:%v err:%v (want created)", created, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read created config: %v", err)
		}
		if !bytes.Equal(got, ConfigTemplate()) {
			t.Fatalf("created config differs from the scaffold")
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("mode = %o, want 640", st.Mode().Perm())
		}
	})

	t.Run("never rewrites an existing config", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\n# operator's file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := ensureConfig(path, 0o640, uid, gid)
		if err != nil || created {
			t.Fatalf("ensureConfig on an existing config = created:%v err:%v (want untouched)", created, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "version: 1\n# operator's file\n" {
			t.Fatalf("existing config was modified: %q", got)
		}
	})

	// A config written earlier by `init` is 0600 root:root. Left that
	// way, the service account cannot read the file the unit is about
	// to hand it and the daemon fails to start on a permission error
	// that names no cause, so provisioning re-asserts the mode and
	// owner the same way it does for the directories.
	t.Run("re-asserts mode on an existing config", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureConfig(path, 0o640, uid, gid); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %o, want 640; the service account cannot read it otherwise", st.Mode().Perm())
		}
	})

	t.Run("refuses a symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.yaml")
		if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "config.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		created, err := ensureConfig(link, 0o640, uid, gid)
		if err == nil || created || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ensureConfig over a symlink = created:%v err:%v (want refusal)", created, err)
		}
	})
}

// TestEnsurePepper covers the pepper step of provisioning: an absent
// pepper is created with a base64 line at 0640 under the requested owner,
// a re-run leaves an existing pepper untouched, and a non-regular file at
// the path is refused.
func TestEnsurePepper(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()

	t.Run("creates a valid pepper", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "auth.pepper")
		created, err := ensurePepper(path, uid, gid)
		if err != nil || !created {
			t.Fatalf("ensurePepper = created:%v err:%v (want created)", created, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSuffix(string(data), "\n")
		if strings.Contains(line, "\n") {
			t.Fatalf("pepper must be a single line, got %q", line)
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			t.Fatalf("pepper line is not base64: %v", err)
		}
		if len(raw) != auth.PepperBytes {
			t.Fatalf("decoded pepper is %d bytes, want %d", len(raw), auth.PepperBytes)
		}
		if len(line) < 16 {
			t.Fatalf("pepper line is %d bytes, below the 16-byte minimum load would reject", len(line))
		}
		if _, err := auth.LoadPepper(path); err != nil {
			t.Fatalf("generated pepper does not load through auth.LoadPepper: %v", err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("mode = %o, want 640", st.Mode().Perm())
		}
	})

	t.Run("never rewrites an existing pepper", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "auth.pepper")
		orig := "a pre-existing operator pepper of plenty of length\n"
		if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := ensurePepper(path, uid, gid)
		if err != nil || created {
			t.Fatalf("ensurePepper on an existing pepper = created:%v err:%v (want untouched)", created, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != orig {
			t.Fatalf("existing pepper was modified: %q", got)
		}
	})

	t.Run("refuses a symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.pepper")
		if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "auth.pepper")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		created, err := ensurePepper(link, uid, gid)
		if err == nil || created || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ensurePepper over a symlink = created:%v err:%v (want refusal)", created, err)
		}
	})
}

// TestEnsureUsers covers the key-store stub step: the absent file becomes
// the stub at 0640, and the stub round-trips auth.LoadUsers — i.e. the
// daemon can load it out of the box. Re-runs and symlinks follow the same
// shape as TestEnsurePepper.
func TestEnsureUsers(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()

	t.Run("creates a stub the daemon can load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "users.yaml")
		created, err := ensureUsers(path, uid, gid)
		if err != nil || !created {
			t.Fatalf("ensureUsers = created:%v err:%v (want created)", created, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != usersStub {
			t.Fatalf("created users file differs from the stub:\n%s", got)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("mode = %o, want 640", st.Mode().Perm())
		}
		uf, err := auth.LoadUsers(path)
		if err != nil {
			t.Fatalf("stub does not round-trip auth.LoadUsers: %v", err)
		}
		if uf.Version != 1 || len(uf.Keys) != 0 {
			t.Fatalf("loaded stub = version %d, %d keys (want 1, 0)", uf.Version, len(uf.Keys))
		}
	})

	t.Run("never rewrites an existing users file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "users.yaml")
		if err := os.WriteFile(path, []byte(usersStub), 0o640); err != nil {
			t.Fatal(err)
		}
		created, err := ensureUsers(path, uid, gid)
		if err != nil || created {
			t.Fatalf("ensureUsers on an existing users file = created:%v err:%v (want untouched)", created, err)
		}
	})

	t.Run("refuses a symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.yaml")
		if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "users.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		created, err := ensureUsers(link, uid, gid)
		if err == nil || created || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ensureUsers over a symlink = created:%v err:%v (want refusal)", created, err)
		}
	})
}

// TestScaffoldFailsValidationUntilCompleted proves the scaffold is
// well-formed YAML (it decodes cleanly) that validation rejects on exactly
// the operator's remaining work — backends and models — so `config check`
// guides the edit and serve stays fail-closed until it is filled in.
// TestScaffoldLineCap keeps the scaffold a stub, not a manual: it is
// capped at 80 non-empty lines, and `config show-effective` — not this
// file — is the field reference.
func TestScaffoldLineCap(t *testing.T) {
	nonEmpty := 0
	for line := range strings.SplitSeq(string(ConfigTemplate()), "\n") {
		if strings.TrimSpace(line) != "" {
			nonEmpty++
		}
	}
	if nonEmpty > 80 {
		t.Errorf("scaffold is %d non-empty lines, want <= 80: keep prose in docs, not in the operator-facing file", nonEmpty)
	}
}

// TestScaffoldHasNoPlanReferences: the scaffold is written to operator
// hosts that have no repository; PLAN citations belong in the repo, not
// in a file an operator edits.
func TestScaffoldHasNoPlanReferences(t *testing.T) {
	if strings.Contains(string(ConfigTemplate()), "PLAN") {
		t.Error("scaffold references PLAN; the operator-facing file must not cite the design document")
	}
}

func TestScaffoldFailsValidationUntilCompleted(t *testing.T) {
	_, err := config.Parse(ConfigTemplate())
	if err == nil {
		t.Fatal("scaffold passed validation: it must require backends and models before serve can start")
	}
	s := err.Error()
	if !strings.Contains(s, "servers") || !strings.Contains(s, "models") {
		t.Fatalf("scaffold validation error does not point at the missing sections: %v", err)
	}
}

func TestScaffoldCompletedExample(t *testing.T) {
	// Uncommenting the two example entries should be enough to validate.
	completed := strings.ReplaceAll(string(ConfigTemplate()), "\n  # ", "\n  ")
	cfg, err := config.Parse([]byte(completed))
	if err != nil {
		t.Fatalf("completed scaffold: %v", err)
	}
	if cfg.Security.Landlock.Mode != "best-effort" || cfg.Security.Landlock.MinimumABI != landlock.DefaultMinimumABI ||
		cfg.Security.BackendNetwork.Mode != "loopback-only" {
		t.Fatalf("scaffold security settings changed: %+v", cfg.Security)
	}
}

func TestEnsureDir(t *testing.T) {
	t.Run("creates", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "sub")
		uid, gid := os.Getuid(), os.Getgid()
		if err := ensureDir(target, 0o750, uid, gid); err != nil {
			t.Fatalf("ensureDir: %v", err)
		}
		st, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat created dir: %v", err)
		}
		if !st.IsDir() {
			t.Fatalf("expected directory, got %v", st.Mode())
		}
		if st.Mode().Perm() != 0o750 {
			t.Errorf("mode = %o, want 750", st.Mode().Perm())
		}
	})

	t.Run("refuses non-directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "plain")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ensureDir(file, 0o750, os.Getuid(), os.Getgid())
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("expected not-a-directory error, got %v", err)
		}
	})

	t.Run("refuses symlink to directory", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		err := ensureDir(link, 0o750, os.Getuid(), os.Getgid())
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("expected symlink refusal, got %v", err)
		}
	})
}

func TestWriteFileAtomic(t *testing.T) {
	t.Run("writes", func(t *testing.T) {
		base := t.TempDir()
		path := filepath.Join(base, "sub", "unit.service")
		if err := writeFileAtomic("hello", path, 0o644); err != nil {
			t.Fatalf("writeFileAtomic: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "hello" {
			t.Errorf("content = %q, want hello", got)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Errorf("mode = %o, want 644", st.Mode().Perm())
		}
	})

	t.Run("refuses symlink destination", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "real")
		if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := writeFileAtomic("evil", link, 0o644)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("expected symlink refusal, got %v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "original" {
			t.Errorf("symlink target was modified: %q", got)
		}
	})
}

// authFixture builds a temp directory with a config (and optional auth
// paths) and returns the config/users/pepper paths and the effective gid.
func authFixture(t *testing.T, configBody string) (cfgPath, usersPath, pepperPath string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	if configBody != "" {
		if err := os.WriteFile(cfgPath, []byte(configBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	usersPath = filepath.Join(dir, "users.yaml")
	pepperPath = filepath.Join(dir, "auth.pepper")
	return cfgPath, usersPath, pepperPath
}

// TestEnsureAuthArtifactsFreshConfig: a freshly written scaffold always
// gets the fixed default pepper and empty users file.
func TestEnsureAuthArtifactsFreshConfig(t *testing.T) {
	cfg, users, pepper := authFixture(t, "")
	uid, gid := os.Getuid(), os.Getgid()
	pc, uc, err := ensureAuthArtifacts(true, cfg, users, pepper, uid, gid)
	if err != nil {
		t.Fatalf("ensureAuthArtifacts: %v", err)
	}
	if !pc || !uc {
		t.Fatalf("fresh config created pepper:%v users:%v (want both)", pc, uc)
	}
	for _, p := range []string{users, pepper} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o640 {
			t.Errorf("%s state = %v err:%v (want a 0640 file)", p, st, err)
		}
	}
}

// TestEnsureAuthArtifactsRetryPartialFresh: an existing config that
// explicitly names the fixed default auth paths creates only the missing
// artifact on a retry (simulating a partial fresh install that crashed
// between pepper and users).
func TestEnsureAuthArtifactsRetryPartialFresh(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	dir := t.TempDir()
	users := filepath.Join(dir, "users.yaml")
	pepper := filepath.Join(dir, "auth.pepper")
	cfg := filepath.Join(dir, "config.yaml")
	body := "version: 1\nauth:\n  users_file: " + users + "\n  pepper_file: " + pepper + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a partial install: the pepper already exists, the users file is missing.
	if err := os.WriteFile(pepper, []byte("a pre-existing pepper line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc, uc, err := ensureAuthArtifacts(false, cfg, users, pepper, uid, gid)
	if err != nil {
		t.Fatalf("ensureAuthArtifacts: %v", err)
	}
	if pc {
		t.Errorf("existing pepper was recreated (pepperCreated=%v)", pc)
	}
	if !uc {
		t.Fatalf("missing users artifact named at its default path was not created")
	}
	if _, err := os.Stat(users); err != nil {
		t.Fatalf("users file missing after retry: %v", err)
	}
	if b, _ := os.ReadFile(pepper); string(b) != "a pre-existing pepper line\n" {
		t.Errorf("existing pepper bytes were rewritten: %q", b)
	}
}

// TestEnsureAuthArtifactsCopiedScaffold: a copied or older scaffold that
// names the exact default auth paths follows the same path-based rule. The
// shipped scaffold names the real /etc defaults (asserted here), and a
// scaffold-like config naming the provisioning default paths creates the
// missing artifacts.
func TestEnsureAuthArtifactsCopiedScaffold(t *testing.T) {
	// The shipped scaffold explicitly names the real fixed default paths,
	// so an operator who copies it before running install follows the
	// path-based rule and gets the missing artifacts.
	u, pep, err := referencedAuthPaths(filepath.Join("..", "..", "deploy", "mellomting-config.yaml.example"))
	if err != nil {
		t.Fatalf("parse shipped scaffold: %v", err)
	}
	if u != "/etc/mellomting/users.yaml" || pep != "/etc/mellomting/auth.pepper" {
		t.Fatalf("shipped scaffold references %q, %q (want the fixed defaults)", u, pep)
	}

	uid, gid := os.Getuid(), os.Getgid()
	cfg, users, pepper := authFixture(t, "")
	// Write a scaffold-style config naming the same paths provisioning
	// treats as its defaults, then confirm the missing artifacts appear.
	body := "version: 1\nauth:\n  users_file: " + users + "\n  pepper_file: " + pepper + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pc, uc, err := ensureAuthArtifacts(false, cfg, users, pepper, uid, gid)
	if err != nil {
		t.Fatalf("ensureAuthArtifacts: %v", err)
	}
	if !pc || !uc {
		t.Fatalf("scaffold-like config created pepper:%v users:%v (want both missing artifacts)", pc, uc)
	}
}

// TestEnsureAuthArtifactsCustomConfigCreatesNothing: a customized or
// unrelated existing config that does not name the fixed default paths
// creates no auth artifact.
func TestEnsureAuthArtifactsCustomConfigCreatesNothing(t *testing.T) {
	cases := []string{
		"version: 1\nauth:\n  users_file: /custom/users.yaml\n  pepper_file: /custom/auth.pepper\n",
		"version: 1\nserver:\n  listen:\n    network: unix\n    address: /run/mellomting/mellomting.sock\n",
	}
	uid, gid := os.Getuid(), os.Getgid()
	for _, body := range cases {
		cfg, users, pepper := authFixture(t, body)
		pc, uc, err := ensureAuthArtifacts(false, cfg, users, pepper, uid, gid)
		if err != nil {
			t.Fatalf("ensureAuthArtifacts: %v", err)
		}
		if pc || uc {
			t.Fatalf("custom config created pepper:%v users:%v (want neither)", pc, uc)
		}
		if _, err := os.Stat(users); !os.IsNotExist(err) {
			t.Errorf("users file should not exist, stat err=%v", err)
		}
		if _, err := os.Stat(pepper); !os.IsNotExist(err) {
			t.Errorf("pepper file should not exist, stat err=%v", err)
		}
	}
}

// TestEnsureAuthArtifactsUnparseableConfigCreatesNothing: a config that
// fails to parse creates no auth files (D15).
func TestEnsureAuthArtifactsUnparseableConfigCreatesNothing(t *testing.T) {
	cfg, users, pepper := authFixture(t, "not: [valid: yaml")
	uid, gid := os.Getuid(), os.Getgid()
	pc, uc, err := ensureAuthArtifacts(false, cfg, users, pepper, uid, gid)
	if err != nil {
		t.Fatalf("ensureAuthArtifacts: %v", err)
	}
	if pc || uc {
		t.Fatalf("unparseable config created pepper:%v users:%v (want neither)", pc, uc)
	}
}

// TestEnsureAuthArtifactsSymlinkConfigCreatesNothing: a config that is a
// symlink (or otherwise non-regular) is refused and creates nothing.
func TestEnsureAuthArtifactsSymlinkConfigCreatesNothing(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(real, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(real, cfgPath); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(dir, "users.yaml")
	pepper := filepath.Join(dir, "auth.pepper")
	pc, uc, err := ensureAuthArtifacts(false, cfgPath, users, pepper, uid, gid)
	if err != nil {
		t.Fatalf("ensureAuthArtifacts: %v", err)
	}
	if pc || uc {
		t.Fatalf("symlink config created pepper:%v users:%v (want neither)", pc, uc)
	}
	if _, err := os.Stat(users); !os.IsNotExist(err) {
		t.Errorf("users file should not exist, stat err=%v", err)
	}
	if _, err := os.Stat(pepper); !os.IsNotExist(err) {
		t.Errorf("pepper file should not exist, stat err=%v", err)
	}
}

// TestReferencedAuthPaths covers the tolerant source parse that finds the
// auth files an existing config names.
func TestReferencedAuthPaths(t *testing.T) {
	t.Run("names both defaults", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		body := "version: 1\nauth:\n  users_file: /etc/mellomting/users.yaml\n  pepper_file: /etc/mellomting/auth.pepper\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		u, pep, err := referencedAuthPaths(p)
		if err != nil {
			t.Fatal(err)
		}
		if u != "/etc/mellomting/users.yaml" || pep != "/etc/mellomting/auth.pepper" {
			t.Fatalf("referenced paths = %q, %q", u, pep)
		}
	})
	t.Run("no auth section", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("version: 1\nserver: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		u, pep, err := referencedAuthPaths(p)
		if err != nil {
			t.Fatal(err)
		}
		if u != "" || pep != "" {
			t.Fatalf("referenced paths = %q, %q (want empty)", u, pep)
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("not: [valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := referencedAuthPaths(p); err == nil {
			t.Fatal("unparseable config returned no error")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		if err := os.WriteFile(real, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "config.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := referencedAuthPaths(link); err == nil {
			t.Fatal("symlink config returned no error")
		}
	})
}

// The unit hardcodes Group=mellomting, so that is the group every file
// the daemon reads must belong to. Owning them to the account's primary
// group instead is correct only while the two agree: an account that
// already existed with a different primary group (a package manager's,
// or useradd without --user-group) left root:root-equivalent ownership
// the running service could not read.
func TestServiceGroupIDPrefersTheNamedGroup(t *testing.T) {
	// A group that exists on every unix: root/wheel by gid 0. Resolving
	// by name must win over the account's primary gid.
	g, err := user.LookupGroupId("0")
	if err != nil {
		t.Skip("no group with gid 0 in the group database")
	}
	account := &user.User{Uid: "1234", Gid: "5678"}
	gid, err := serviceGroupID(g.Name, account)
	if err != nil {
		t.Fatal(err)
	}
	if gid != 0 {
		t.Fatalf("gid = %d, want 0 (the group the unit names, not the account's primary %s)", gid, account.Gid)
	}
}

// A named group that does not exist is fail-closed: the unit would not
// start, so provisioning says so instead of owning the files to a group
// the daemon never runs as.
func TestServiceGroupIDMissingGroupIsAnError(t *testing.T) {
	account := &user.User{Uid: "1234", Gid: "5678"}
	_, err := serviceGroupID("mellomting-no-such-group-9f2a", account)
	if err == nil {
		t.Fatal("a missing service group was accepted")
	}
	if !strings.Contains(err.Error(), "no such group exists") {
		t.Fatalf("err = %v, want it to name the missing group", err)
	}
}
