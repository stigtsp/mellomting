package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mellomting/internal/version"
)

// writeSourceBinary creates a fake "source binary" with known content and
// a non-world-executable mode, so the tests can verify that install
// installs 0755 explicitly rather than copying the source mode.
func writeSourceBinary(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "source-binary")
	if err := os.WriteFile(src, []byte("fake-mellomting-binary-bytes\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestInstallBinaryInstallsExecutableCopy(t *testing.T) {
	src := writeSourceBinary(t)
	prefix := filepath.Join(t.TempDir(), "prefix")
	dest := filepath.Join(prefix, "bin", version.Name)
	if err := installBinary(src, dest); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("installed content = %q, want %q", got, want)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %v (want 0755)", st.Mode())
	}
	entries, err := os.ReadDir(filepath.Join(prefix, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != version.Name {
		t.Fatalf("bin dir entries = %v (want exactly %q, no temp leftovers)", entries, version.Name)
	}
}

func TestInstallBinaryRefusesSymlinkDestination(t *testing.T) {
	src := writeSourceBinary(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("do-not-touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(bin, version.Name)
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	err := installBinary(src, dest)
	if err == nil {
		t.Fatal("installBinary over a symlink destination succeeded (want refusal)")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("refusal error = %v (want a symlink mention)", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "do-not-touch\n" {
		t.Fatalf("symlink target was modified: %q (%v)", b, err)
	}
	st, err := os.Lstat(dest)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink destination was replaced: st=%v err=%v", st, err)
	}
}

func TestInstallBinaryRefusesDirectoryDestination(t *testing.T) {
	src := writeSourceBinary(t)
	prefix := filepath.Join(t.TempDir(), "prefix")
	dest := filepath.Join(prefix, "bin", version.Name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := installBinary(src, dest)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("installBinary over a directory destination = %v (want refusal)", err)
	}
}

func TestInstallBinaryReplacesExistingAndResetsMode(t *testing.T) {
	src := writeSourceBinary(t)
	prefix := filepath.Join(t.TempDir(), "prefix")
	dest := filepath.Join(prefix, "bin", version.Name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("very old content"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "fake-mellomting-binary") || strings.Contains(string(got), "very old content") {
		t.Fatalf("installed content = %q (want the new source bytes)", got)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode after replace = %v (want 0755)", st.Mode())
	}
}

func TestInstallBinaryAlreadyInstalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), version.Name)
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(path, path); !errors.Is(err, errAlreadyInstalled) {
		t.Fatalf("installBinary(src=dest) = %v (want errAlreadyInstalled)", err)
	}
}

// TestInstallBinaryNoOpThroughSymlinkedPrefix is the macOS /var case:
// the destination's prefix reaches the same physical file through a
// symlink, so a literal-path comparison misses the no-op.
func TestInstallBinaryNoOpThroughSymlinkedPrefix(t *testing.T) {
	base := t.TempDir()
	realPrefix := filepath.Join(base, "real")
	dest := filepath.Join(realPrefix, "bin", version.Name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPrefix := filepath.Join(base, "link")
	if err := os.MkdirAll(linkPrefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPrefix, filepath.Join(linkPrefix, "real")); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(dest, filepath.Join(linkPrefix, "real", "bin", version.Name)); !errors.Is(err, errAlreadyInstalled) {
		t.Fatalf("installBinary through a symlinked prefix = %v (want errAlreadyInstalled)", err)
	}
}

func TestSameResolvedPath(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real", version.Name)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if !sameResolvedPath(real, filepath.Join(base, "link", version.Name)) {
		t.Fatal("sameResolvedPath through a symlinked component = false (want true)")
	}
	if sameResolvedPath(real, filepath.Join(base, "missing")) {
		t.Fatal("sameResolvedPath with a missing second path = true (want false)")
	}
}

func TestPerformInstallFresh(t *testing.T) {
	src := writeSourceBinary(t)
	prefix := filepath.Join(t.TempDir(), "prefix")
	code, out, errOut := captureOutput(t, func() int {
		_, c := performInstall(src, prefix)
		return c
	})
	if code != 0 {
		t.Fatalf("performInstall exit = %d, stderr: %s", code, errOut)
	}
	dest := filepath.Join(prefix, "bin", version.Name)
	if !strings.Contains(out, "installed "+version.Version+" at "+dest) {
		t.Fatalf("stdout = %q (want installed %s at %s)", out, version.Version, dest)
	}
	if !strings.Contains(out, "note: the daemon creates neither its config nor its log/runtime directories") {
		t.Fatalf("stdout = %q (want the config/log/runtime directory note)", out)
	}
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed file state = st:%v err:%v (want a 0755 file)", st, err)
	}
}

func TestPerformInstallTrailingSlashPrefix(t *testing.T) {
	src := writeSourceBinary(t)
	base := t.TempDir()
	code, _, errOut := captureOutput(t, func() int {
		_, c := performInstall(src, base+"/")
		return c
	})
	if code != 0 {
		t.Fatalf("performInstall exit = %d, stderr: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(base, "bin", version.Name)); err != nil {
		t.Fatalf("installed file missing: %v", err)
	}
}

func TestPerformInstallAlreadyInstalled(t *testing.T) {
	prefix := t.TempDir()
	dest := filepath.Join(prefix, "bin", version.Name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := captureOutput(t, func() int {
		_, c := performInstall(dest, prefix)
		return c
	})
	if code != 0 {
		t.Fatalf("performInstall no-op exit = %d (want 0)", code)
	}
	if !strings.Contains(out, "already installed at "+dest) {
		t.Fatalf("stdout = %q (want already installed at %s)", out, dest)
	}
	if strings.Contains(out, "log/runtime directories") {
		t.Fatalf("stdout = %q (the no-op install must not repeat the directory note)", out)
	}
}

func TestInstallCmdUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--bogus"}},
		{"unexpected positional", []string{"extra"}},
		{"empty prefix", []string{"--prefix", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := captureOutput(t, func() int {
				return installCmd(tc.args)
			})
			if code != 2 {
				t.Fatalf("installCmd %v exit = %d (want 2)", tc.args, code)
			}
		})
	}
}

// TestInstallCmdSystemdFailClosedBeforeWrite proves --systemd fails closed
// before the binary is written when the host cannot provision. Regression:
// previously the binary landed at <prefix>/bin/, then the systemd half
// failed with "requires a Linux host", leaving a half-install behind. On a
// provisionable Linux root host the command would mutate /etc, so that
// path is exercised on a real host, not in a unit test.
func TestInstallCmdSystemdFailClosedBeforeWrite(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("host is Linux; provisioning mutates /etc and is exercised on a real Linux root host")
	}
	prefix := filepath.Join(t.TempDir(), "prefix")
	code, out, _ := captureOutput(t, func() int {
		return installCmd([]string{"--systemd", "--prefix", prefix})
	})
	if code != 1 {
		t.Fatalf("installCmd --systemd exit = %d (want 1: fail closed on non-Linux), stdout=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", version.Name)); !os.IsNotExist(err) {
		t.Fatalf("binary was written before the systemd preflight failed (want not found): %v", err)
	}
}
