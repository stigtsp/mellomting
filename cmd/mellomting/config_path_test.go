package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirIn moves the process working directory to dir for the duration of
// the test and restores it afterwards.
func chdirIn(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// TestResolveConfigPathExplicit pins D1: an explicit --config PATH wins and
// an explicitly named missing path reports that path and never falls back.
func TestResolveConfigPathExplicit(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(explicit, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveConfigPath(explicit)
	if err != nil {
		t.Fatalf("ResolveConfigPath(%q) error: %v", explicit, err)
	}
	if got != explicit {
		t.Fatalf("ResolveConfigPath(%q) = %q, want %q", explicit, got, explicit)
	}

	missing := filepath.Join(dir, "does-not-exist.yaml")
	got, err = ResolveConfigPath(missing)
	if err != nil {
		t.Fatalf("ResolveConfigPath(missing) error: %v", err)
	}
	if got != missing {
		t.Fatalf("ResolveConfigPath(missing) = %q, want %q (no fallback)", got, missing)
	}
}

// TestResolveConfigPathCwdWins pins D1: a regular ./config.yaml in the
// working directory wins over /etc.
func TestResolveConfigPathCwdWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, dir)
	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatalf("ResolveConfigPath error: %v", err)
	}
	if got != "./config.yaml" {
		t.Fatalf("ResolveConfigPath = %q, want ./config.yaml", got)
	}
}

// TestResolveConfigPathFallsBack pins D1: with no ./config.yaml present the
// resolver returns the system default path.
func TestResolveConfigPathFallsBack(t *testing.T) {
	dir := t.TempDir()
	chdirIn(t, dir)
	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatalf("ResolveConfigPath error: %v", err)
	}
	if got != "/etc/mellomting/config.yaml" {
		t.Fatalf("ResolveConfigPath = %q, want /etc/mellomting/config.yaml", got)
	}
}

// TestResolveConfigPathNonRegular pins D1: a local config.yaml that exists
// as a symlink (including dangling), directory, or other non-regular file
// fails resolution rather than falling through to /etc.
func TestResolveConfigPathNonRegular(t *testing.T) {
	dangling := t.TempDir()
	chdirIn(t, dangling)
	if err := os.Symlink(filepath.Join(dangling, "missing-target.yaml"), filepath.Join(dangling, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConfigPath(""); err == nil {
		t.Fatal("dangling symlink config.yaml: want error, got nil")
	}

	realSymlink := t.TempDir()
	target := filepath.Join(realSymlink, "real.yaml")
	if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(realSymlink, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, realSymlink)
	if _, err := ResolveConfigPath(""); err == nil {
		t.Fatal("symlink config.yaml: want error, got nil")
	}

	dirAsConfig := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirAsConfig, "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, dirAsConfig)
	if _, err := ResolveConfigPath(""); err == nil {
		t.Fatal("directory config.yaml: want error, got nil")
	}
}

// TestConfigHelpNoLookup pins D1: --help performs no config-path lookup and
// reports a usage error (exit 2) for every config-dependent command. This
// guards against an accidental lookup that would fail on a cwd with a
// hostile ./config.yaml.
func TestConfigHelpNoLookup(t *testing.T) {
	hostile := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(hostile, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, hostile)

	commands := map[string]func([]string) int{
		"config check":          func(a []string) int { return configCmd(append([]string{"check"}, a...)) },
		"config show-effective": func(a []string) int { return configCmd(append([]string{"show-effective"}, a...)) },
		"usage report":          func(a []string) int { return usageCmd(append([]string{"report"}, a...)) },
		"serve":                 serveCmd,
		"sandbox check":         func(a []string) int { return sandboxCmd(append([]string{"check"}, a...)) },
		"key create":            func(a []string) int { _, code := keyParseFlags("create", a); return code },
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			code, _, _ := captureOutput(t, func() int { return run([]string{"--help"}) })
			if code != 2 {
				t.Fatalf("%s --help exit = %d (want 2)", name, code)
			}
		})
	}
}

// TestConfigFlagPostSubcommand pins D1: --config is accepted at the
// post-command or post-subcommand position and routes to the given path.
func TestConfigFlagPostSubcommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yaml")
	cfgContent := `version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/x.sock
    mode: "0660"
auth:
  users_file: ` + dir + `/users.yaml
  pepper_file: ` + dir + `/auth.pepper
servers:
  a:
    url: http://127.0.0.1:8001
models:
  m:
    type: generation
    strategy: single
    upstream_model: m
    servers: [a]
`
	if err := os.WriteFile(cfg, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, t.TempDir()) // no ./config.yaml here

	code, out, _ := captureOutput(t, func() int {
		return configCmd([]string{"check", "--config", cfg})
	})
	if code != 0 {
		t.Fatalf("config check --config exit = %d", code)
	}
	if !strings.Contains(out, cfg) {
		t.Fatalf("config check --config output = %q (want resolved path %q)", out, cfg)
	}
}

// TestConfigRootLevelFlagRejected pins D1: a root-level --config is not a
// recognized command and is rejected at dispatch time. Exercised against the
// real binary so os.Exit paths are covered.
func TestConfigRootLevelFlagRejected(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	code, _, errOut := runCLI(t, bin, dir, "--config", filepath.Join(dir, "x.yaml"), "serve")
	if code == 0 {
		t.Fatalf("root-level --config accepted (exit 0)")
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Fatalf("root-level --config stderr = %q (want unknown command)", errOut)
	}
}
