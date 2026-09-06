package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"mellomting/internal/config"
)

const cliTestCredentialSecret = "credential-content-must-not-appear"

// captureOutput runs fn while os.Stdout and os.Stderr are redirected to
// in-memory pipes, and returns the exit code plus the captured output.
// Output is drained concurrently so a function emitting more than the
// pipe buffer never deadlocks.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr

	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(rOut)
		outCh <- string(b)
	}()
	go func() {
		b, _ := io.ReadAll(rErr)
		errCh <- string(b)
	}()

	code = fn()

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, <-outCh, <-errCh
}

// writeCLIConfig writes a minimal valid configuration (the same shape
// the subprocess fixtures use) and returns its path.
func writeCLIConfig(t *testing.T) string {
	t.Helper()
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
    api_key_file: ` + dir + `/qwen-a.key

models:
  qwen-coder:
    type: generation
    strategy: single
    upstream_model: Qwen/Qwen3-Coder-Next
    servers:
      - qwen-a
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen-a.key"), []byte(cliTestCredentialSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSubcommandAndDescribeUnknownCommand pins the top-level arg
// dispatch helpers: a leading non-flag word is the subcommand, anything
// else falls back to the default, and unknown commands get a stable
// message.
func TestSubcommandAndDescribeUnknownCommand(t *testing.T) {
	sub, rest := subcommand([]string{"check", "-config", "x"}, "list")
	if sub != "check" || len(rest) != 2 || rest[0] != "-config" {
		t.Fatalf("subcommand = %q %v", sub, rest)
	}
	sub, rest = subcommand([]string{"-config", "x"}, "check")
	if sub != "check" || len(rest) != 2 {
		t.Fatalf("default subcommand = %q %v", sub, rest)
	}
	if got := describeUnknownCommand("bogus"); got != `unknown command "bogus"` {
		t.Fatalf("describeUnknownCommand = %q", got)
	}
}

func TestSplitModels(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		// An explicitly empty list would write a key that can reach
		// nothing, silently.
		{in: "", wantErr: true},
		{in: "a", want: []string{"a"}},
		{in: " a , b ", want: []string{"a", "b"}},
		{in: "a,,b", want: []string{"a", "b"}},
		{in: "*", want: []string{"*"}},
		// The wildcard used to swallow every name beside it, so an ACL
		// written to allow "a" allowed everything instead.
		{in: "a,*", wantErr: true},
		{in: "*,a", wantErr: true},
	}
	for _, tc := range cases {
		got, err := splitModels(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("splitModels(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("splitModels(%q): %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("splitModels(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("splitModels(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

func TestConfigCheckValid(t *testing.T) {
	cfgPath := writeCLIConfig(t)
	code, out, _ := captureOutput(t, func() int {
		return configCmd([]string{"check", "-config", cfgPath})
	})
	if code != 0 {
		t.Fatalf("config check exit = %d", code)
	}
	if !strings.Contains(out, "is valid") {
		t.Fatalf("config check output = %q", out)
	}
}

func TestConfigCheckInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	// Missing auth.users_file fails validation fail-closed.
	if err := os.WriteFile(bad, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := captureOutput(t, func() int {
		return configCmd([]string{"check", "-config", bad})
	})
	if code != 1 {
		t.Fatalf("config check exit = %d (want 1)", code)
	}
	if !strings.Contains(errOut, "config check failed") {
		t.Fatalf("config check stderr = %q", errOut)
	}
}

func TestConfigShowEffective(t *testing.T) {
	cfgPath := writeCLIConfig(t)
	code, out, _ := captureOutput(t, func() int {
		return configCmd([]string{"show-effective", "-config", cfgPath})
	})
	if code != 0 {
		t.Fatalf("show-effective exit = %d", code)
	}
	for _, want := range []string{
		"version: 1",
		"servers:",
		"models:",
		"qwen-a",
		"upstream_model: Qwen/Qwen3-Coder-Next",
		"users_file",
		"loopback-only",
		filepath.Join(filepath.Dir(cfgPath), "qwen-a.key"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show-effective missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "auto-") {
		t.Fatalf("show-effective leaked a synthetic backend name:\n%s", out)
	}
	if strings.Contains(out, cliTestCredentialSecret) {
		t.Fatalf("show-effective leaked credential contents:\n%s", out)
	}

	original, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load original: %v", err)
	}
	reparsed, err := config.Parse([]byte(out))
	if err != nil {
		t.Fatalf("effective config did not re-parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(original, reparsed) {
		t.Fatalf("effective config did not round-trip:\noriginal = %+v\nreparsed = %+v", original, reparsed)
	}
}

func TestConfigUnknownSubcommand(t *testing.T) {
	if code, _, _ := captureOutput(t, func() int {
		return configCmd([]string{"frobnicate"})
	}); code != 2 {
		t.Fatalf("config unknown subcommand exit = %d (want 2)", code)
	}
}

func TestSandboxCheckReportOnly(t *testing.T) {
	code, out, _ := captureOutput(t, func() int {
		return sandboxCmd(nil)
	})
	if code != 0 {
		t.Fatalf("sandbox check report-only exit = %d", code)
	}
	// Only lines that print on every platform may be asserted: the ABI
	// lines belong to Landlock, so this test must pass on macOS and on
	// a Linux host without Landlock alike.
	for _, want := range []string{"platform:", "backend:", "result:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sandbox check missing %q:\n%s", want, out)
		}
	}
}

func TestSandboxCheckBadConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := captureOutput(t, func() int {
		return sandboxCmd([]string{"check", "-config", bad})
	})
	if code != 1 {
		t.Fatalf("sandbox check exit = %d (want 1)", code)
	}
	if !strings.Contains(errOut, "sandbox check failed") {
		t.Fatalf("sandbox check stderr = %q", errOut)
	}
}

func TestSandboxUnknownSubcommand(t *testing.T) {
	if code, _, _ := captureOutput(t, func() int {
		return sandboxCmd([]string{"probe"})
	}); code != 2 {
		t.Fatalf("sandbox unknown subcommand exit = %d (want 2)", code)
	}
}

func TestUsageReportAccountingDisabledInProcess(t *testing.T) {
	cfgPath := writeCLIConfig(t) // no accounting block: disabled
	code, _, errOut := captureOutput(t, func() int {
		return usageCmd([]string{"report", "-config", cfgPath})
	})
	if code != 1 {
		t.Fatalf("usage report exit = %d (want 1)", code)
	}
	if !strings.Contains(errOut, "accounting is disabled") {
		t.Fatalf("usage report stderr = %q", errOut)
	}
}

func TestUsageUnknownSubcommandInProcess(t *testing.T) {
	if code, _, _ := captureOutput(t, func() int {
		return usageCmd([]string{"totals"})
	}); code != 2 {
		t.Fatalf("usage unknown subcommand exit = %d (want 2)", code)
	}
}

func TestServeInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := captureOutput(t, func() int {
		return serveCmd([]string{"-config", bad})
	})
	if code != 1 {
		t.Fatalf("serve invalid config exit = %d (want 1)", code)
	}
	if !strings.Contains(errOut, "invalid configuration") {
		t.Fatalf("serve stderr = %q", errOut)
	}
}

func TestServeUnknownFlag(t *testing.T) {
	if code, _, _ := captureOutput(t, func() int {
		return serveCmd([]string{"--bogus"})
	}); code != 2 {
		t.Fatalf("serve unknown flag exit = %d (want 2)", code)
	}
}

func TestKeyParseFlagsErrors(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		args []string
	}{
		{"missing name", "create", nil},
		{"invalid expires", "create", []string{"x", "-models", "m", "-expires", "not-a-date"}},
		{"negative concurrent", "create", []string{"x", "-models", "m", "-concurrent-requests", "-1"}},
		{"negative rps", "create", []string{"x", "-models", "m", "-requests-per-second", "-1"}},
		{"negative burst", "create", []string{"x", "-models", "m", "-burst", "-1"}},
		{"unexpected positional", "create", []string{"x", "-models", "m", "extra"}},
		{"missing id", "revoke", nil},
		{"missing id enable", "enable", nil},
		{"missing id disable", "disable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := captureOutput(t, func() int {
				_, c := keyParseFlags(tc.sub, tc.args)
				return c
			})
			if code != 2 {
				t.Fatalf("key %s %v exit = %d (want 2)", tc.sub, tc.args, code)
			}
		})
	}
}

// TestKeyParseFlagsValid pins the happy path: valid create flags parse
// without error, and --models defaults to the wildcard.
func TestKeyParseFlagsValid(t *testing.T) {
	c, code := keyParseFlags("create", []string{"x", "-models", "a, b", "-concurrent-requests", "4"})
	if code != 0 {
		t.Fatalf("key create valid flags exit = %d", code)
	}
	parsed, err := splitModels(c.models)
	if err != nil {
		t.Fatal(err)
	}
	if c.operand != "x" || c.concurrentRequests != 4 || len(parsed) != 2 {
		t.Fatalf("parsed flags = %+v", c)
	}

	c, code = keyParseFlags("create", []string{"x"})
	if code != 0 {
		t.Fatalf("key create without --models must parse: exit = %d", code)
	}
	if c.models != config.ModelWildcard {
		t.Fatalf("models = %q, want the wildcard default", c.models)
	}
	// A bare date expires at the start of that day, UTC.
	c, code = keyParseFlags("create", []string{"x", "-expires", "2027-03-04"})
	if code != 0 || c.expires == nil || !c.expires.Equal(time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires = %v (exit %d), want 2027-03-04T00:00:00Z", c.expires, code)
	}
}

// `sandbox check` answers what `serve` would do on this host, which is
// the capability and the configured mode together. Reporting that a
// policy will be enforced while the mode says otherwise sends an
// operator away believing a host is confined when nothing confines it.
func TestSandboxCheckResultFollowsMode(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, mode string) string {
		t.Helper()
		section := "landlock"
		if runtime.GOOS == "darwin" {
			section = "seatbelt"
		}
		cfg := fmt.Sprintf(`version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/m.sock
    mode: "0660"
auth:
  users_file: %s/users.yaml
  pepper_file: %s/auth.pepper
security:
  %s:
    mode: %s
servers:
  a:
    url: http://127.0.0.1:8001
models:
  m:
    upstream_model: Q
    servers: [a]
`, dir, dir, section, mode)
		path := filepath.Join(dir, mode+".yaml")
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	code, out, _ := captureOutput(t, func() int {
		return sandboxCmd([]string{"check", "-config", write(t, "disabled")})
	})
	if code != 0 {
		t.Fatalf("disabled exit = %d", code)
	}
	if !strings.Contains(out, "not enforced (mode disabled)") {
		t.Fatalf("a disabled sandbox must not report that it will be enforced:\n%s", out)
	}

	// Whatever this host can do, the answer for an enforcing mode must
	// agree with it rather than with the mode alone.
	code, out, _ = captureOutput(t, func() int {
		return sandboxCmd([]string{"check", "-config", write(t, "best-effort")})
	})
	if code != 0 {
		t.Fatalf("best-effort exit = %d", code)
	}
	enforceable := platformSandbox(&config.Config{}).check().Supported
	if enforceable != strings.Contains(out, "will enforce") {
		t.Fatalf("host enforceable=%v but result says otherwise:\n%s", enforceable, out)
	}
}
