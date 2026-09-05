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
	if got := ResolveConfigPath(explicit); got != explicit {
		t.Fatalf("ResolveConfigPath(%q) = %q, want %q", explicit, got, explicit)
	}

	missing := filepath.Join(dir, "does-not-exist.yaml")
	if got := ResolveConfigPath(missing); got != missing {
		t.Fatalf("ResolveConfigPath(missing) = %q, want %q (no fallback)", got, missing)
	}
}

// TestResolveConfigPathIgnoresCwd pins D1: the working directory is never
// consulted. These commands run as root, so a ./config.yaml planted by
// whoever can write the directory root happens to run from must not be
// picked up — it would choose users_file, pepper_file, the backends, and
// the sandbox mode. Resolution returns the system default even when a
// perfectly valid local config.yaml is present.
func TestResolveConfigPathIgnoresCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chdirIn(t, dir)
	if got := ResolveConfigPath(""); got != "/etc/mellomting/config.yaml" {
		t.Fatalf("ResolveConfigPath = %q, want /etc/mellomting/config.yaml; the cwd must never be consulted", got)
	}
}

// TestResolveConfigPathDefault pins D1: with no --config the resolver
// returns the system default path.
func TestResolveConfigPathDefault(t *testing.T) {
	chdirIn(t, t.TempDir())
	if got := ResolveConfigPath(""); got != "/etc/mellomting/config.yaml" {
		t.Fatalf("ResolveConfigPath = %q, want /etc/mellomting/config.yaml", got)
	}
}

// TestCwdConfigNotPickedUp pins D1 against the real binary: a valid
// config.yaml sitting in the working directory must not be loaded by a
// config-dependent command run without --config. Running the command from
// a directory someone else can write must not hand them the config, which
// selects the auth files, the backends, and the sandbox mode.
func TestCwdConfigNotPickedUp(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	planted := `version: 1
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
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, bin, dir, "config", "check")
	if code == 0 {
		t.Fatalf("config check loaded the working-directory config (exit 0): %s", out)
	}
	if strings.Contains(out+errOut, filepath.Join(dir, "config.yaml")) {
		t.Fatalf("the planted config was resolved: %s", out+errOut)
	}
	if !strings.Contains(out+errOut, "/etc/mellomting/config.yaml") {
		t.Fatalf("want the system default path in the output, got: %s", out+errOut)
	}

	// Naming it explicitly still works.
	code, out, errOut = runCLI(t, bin, dir, "config", "check", "--config", "./config.yaml")
	if code != 0 {
		t.Fatalf("explicit --config ./config.yaml exit = %d: %s%s", code, out, errOut)
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

// Every config-dependent command rejects trailing operands rather than
// silently ignoring them. A path typed without --config used to be
// discarded, so `config check /some/typo.yaml` reported on the default
// configuration instead — a misleading result from a fail-closed
// validation command.
func TestTrailingArgumentsRejected(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	stray := filepath.Join(dir, "typo.yaml")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"config check", []string{"config", "check", stray}},
		{"config show-effective", []string{"config", "show-effective", stray}},
		{"sandbox check", []string{"sandbox", "check", stray}},
		{"serve", []string{"serve", stray}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, bin, dir, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage error): %s%s", code, out, errOut)
			}
			if !strings.Contains(errOut, "unexpected arguments") {
				t.Fatalf("stderr = %q, want an unexpected-arguments message", errOut)
			}
		})
	}
}
