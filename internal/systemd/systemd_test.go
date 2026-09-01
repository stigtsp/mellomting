package systemd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mellomting/internal/config"
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

// TestScaffoldFailsValidationUntilCompleted proves the scaffold is
// well-formed YAML (it decodes cleanly) that validation rejects on exactly
// the operator's remaining work — backends and models — so `config check`
// guides the edit and serve stays fail-closed until it is filled in.
func TestScaffoldFailsValidationUntilCompleted(t *testing.T) {
	_, err := config.Parse(ConfigTemplate())
	if err == nil {
		t.Fatal("scaffold passed validation: it must require backends and models before serve can start")
	}
	s := err.Error()
	if !strings.Contains(s, "backends") || !strings.Contains(s, "models") {
		t.Fatalf("scaffold validation error does not point at the missing sections: %v", err)
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
