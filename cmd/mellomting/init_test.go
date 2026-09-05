package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/discovery"
	"mellomting/internal/landlock"
)

func withInitLandlock(t *testing.T, report landlock.Report) {
	t.Helper()
	old := initLandlockCheck
	initLandlockCheck = func() landlock.Report { return report }
	t.Cleanup(func() { initLandlockCheck = old })
}

func supportedLandlockReport() landlock.Report {
	return landlock.Report{Platform: "linux", Supported: true, KernelABI: landlock.MaxABI}
}

func unsupportedLandlockReport() landlock.Report {
	return landlock.Report{Platform: "linux", Reason: "test-unsupported"}
}

func tooOldLandlockReport() landlock.Report {
	return landlock.Report{Platform: "linux", Supported: true, KernelABI: landlock.DefaultMinimumABI - 1}
}

func TestParseInitServers(t *testing.T) {
	t.Parallel()

	t.Run("sole unnamed server", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitServers([]string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		want := []discovery.Server{{Name: "local", BaseURL: "http://127.0.0.1:8000"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("multiple unnamed servers", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitServers([]string{"http://127.0.0.1:8000", "http://127.0.0.1:8001"})
		if err != nil {
			t.Fatal(err)
		}
		want := []discovery.Server{
			{Name: "local-1", BaseURL: "http://127.0.0.1:8000"},
			{Name: "local-2", BaseURL: "http://127.0.0.1:8001"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("explicit names", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitServers([]string{"a=http://127.0.0.1:8000", "b-2=http://127.0.0.1:8001"})
		if err != nil {
			t.Fatal(err)
		}
		want := []discovery.Server{
			{Name: "a", BaseURL: "http://127.0.0.1:8000"},
			{Name: "b-2", BaseURL: "http://127.0.0.1:8001"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("mixed named and unnamed servers", func(t *testing.T) {
		t.Parallel()
		_, err := parseInitServers([]string{"a=http://127.0.0.1:8000", "http://127.0.0.1:8001"})
		if err == nil || !strings.Contains(err.Error(), "all servers must have names") {
			t.Fatalf("err = %v, want naming rule", err)
		}
	})

	cases := []struct {
		name   string
		args   []string
		errSub string
	}{
		{"empty name", []string{"=http://127.0.0.1:8000"}, "must match"},
		{"uppercase name", []string{"A=http://127.0.0.1:8000"}, "must match"},
		{"name starts with digit", []string{"1a=http://127.0.0.1:8000"}, "must match"},
		{"name too long", []string{strings.Repeat("a", 64) + "=http://127.0.0.1:8000"}, "must match"},
		{"duplicate names", []string{"a=http://127.0.0.1:8000", "a=http://127.0.0.1:8001"}, "duplicate name"},
		{"hostname rejected", []string{"http://example.com:8000"}, "literal IP"},
		{"missing port", []string{"http://127.0.0.1"}, "explicit port"},
		{"port zero", []string{"http://127.0.0.1:0"}, "port"},
		{"port too large", []string{"http://127.0.0.1:65536"}, "port"},
		{"path rejected", []string{"http://127.0.0.1:8000/v1"}, "path"},
		{"root path rejected", []string{"http://127.0.0.1:8000/"}, "path"},
		{"query rejected", []string{"http://127.0.0.1:8000?x=1"}, "query"},
		{"fragment rejected", []string{"http://127.0.0.1:8000#f"}, "fragment"},
		{"userinfo rejected", []string{"http://user:pass@127.0.0.1:8000"}, "userinfo"},
		{"bad scheme", []string{"ftp://127.0.0.1:8000"}, "scheme"},
		{"duplicate destination", []string{"http://127.0.0.1:8000", "http://[::ffff:127.0.0.1]:8000"}, "duplicate canonical destination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseInitServers(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want %q", err, tc.errSub)
			}
		})
	}

	t.Run("maximum server count", func(t *testing.T) {
		t.Parallel()
		args := make([]string, discovery.MaxServers)
		for i := range args {
			args[i] = fmt.Sprintf("s%d=http://127.0.0.1:%d", i, 9000+i)
		}
		if _, err := parseInitServers(args); err != nil {
			t.Fatalf("max servers: %v", err)
		}
		args = append(args, fmt.Sprintf("s%d=http://127.0.0.1:%d", discovery.MaxServers, 9000+discovery.MaxServers))
		if _, err := parseInitServers(args); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", discovery.MaxServers)) {
			t.Fatalf("err = %v, want server bound", err)
		}
	})
}

func TestParseInitListen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want initListener
	}{
		{"IPv4 loopback", "127.0.0.1:8080", initListener{Network: "tcp", Address: "127.0.0.1:8080", Host: "127.0.0.1", Port: 8080}},
		{"IPv6 loopback", "[::1]:8080", initListener{Network: "tcp", Address: "[::1]:8080", Host: "::1", Port: 8080}},
		{"IPv4 non-loopback", "192.168.1.20:8080", initListener{Network: "tcp", Address: "192.168.1.20:8080", Host: "192.168.1.20", Port: 8080, AllowPlaintext: true}},
		{"IPv4 wildcard", "0.0.0.0:8080", initListener{Network: "tcp", Address: "0.0.0.0:8080", Host: "0.0.0.0", Port: 8080, AllowPlaintext: true}},
		{"IPv6 wildcard", "[::]:8080", initListener{Network: "tcp", Address: "[::]:8080", Host: "::", Port: 8080, AllowPlaintext: true}},
		{"empty host", ":8080", initListener{Network: "tcp", Address: ":8080", Port: 8080, AllowPlaintext: true}},
		{"Unix socket", "/absolute/path/mellomting.sock", initListener{Network: "unix", Address: "/absolute/path/mellomting.sock", Path: "/absolute/path/mellomting.sock"}},
		{"leading zero port", ":08080", initListener{Network: "tcp", Address: ":8080", Port: 8080, AllowPlaintext: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseInitListen(tc.raw)
			if err != nil {
				t.Fatalf("parseInitListen(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	errCases := []struct {
		name   string
		raw    string
		errSub string
	}{
		{"empty", "", "required"},
		{"bare port", "8080", "host:port"},
		{"relative Unix path", "relative.sock", "host:port"},
		{"hostname", "example.com:8080", "literal IP"},
		{"URL scheme", "http://127.0.0.1:8080", "host:port"},
		{"invalid port zero", ":0", "port"},
		{"invalid port too large", ":65536", "port"},
		{"invalid port text", ":abc", "port"},
		{"invalid port sign", ":+1", "port"},
		{"unbracketed IPv6", "::1:8080", "bracketed"},
		{"zone-scoped IPv6", "[fe80::1%eth0]:8080", "literal IP"},
		{"uncleaned Unix path", "/a/../b", "cleaned"},
		{"root Unix path", "/", "cleaned"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseInitListen(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want %q", err, tc.errSub)
			}
		})
	}
}

func TestParseInitArguments(t *testing.T) {
	t.Parallel()

	t.Run("missing servers", func(t *testing.T) {
		t.Parallel()
		_, err := parseInitArguments("", defaultInitListen, "required", false, false, nil)
		if err == nil || !strings.Contains(err.Error(), "example: mellomting init --server http://127.0.0.1:8000") {
			t.Fatalf("err = %v, want example", err)
		}
	})

	t.Run("fixed auth filenames", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitArguments("/var/mellomting/app/config.yaml", defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigPath != "/var/mellomting/app/config.yaml" ||
			got.UsersPath != "/var/mellomting/app/users.yaml" ||
			got.PepperPath != "/var/mellomting/app/auth.pepper" {
			t.Fatalf("paths = %q %q %q", got.ConfigPath, got.UsersPath, got.PepperPath)
		}
	})

	t.Run("default config path", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitArguments("", defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigPath != config.DefaultConfigPath ||
			got.UsersPath != filepath.Join(filepath.Dir(config.DefaultConfigPath), "users.yaml") ||
			got.PepperPath != filepath.Join(filepath.Dir(config.DefaultConfigPath), "auth.pepper") {
			t.Fatalf("default paths = %q %q %q", got.ConfigPath, got.UsersPath, got.PepperPath)
		}
	})

	t.Run("invalid landlock", func(t *testing.T) {
		t.Parallel()
		_, err := parseInitArguments("", defaultInitListen, "off", true, false, []string{"http://127.0.0.1:8000"})
		if err == nil || !strings.Contains(err.Error(), "--landlock must be") {
			t.Fatalf("err = %v, want landlock", err)
		}
	})

	t.Run("listener default preserved", func(t *testing.T) {
		t.Parallel()
		got, err := parseInitArguments("", "", "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatalf("empty --listen should use default: %v", err)
		}
		if got.Listener.Address != defaultInitListen {
			t.Fatalf("listener = %q", got.Listener.Address)
		}
	})
}

func TestInitPreflightLandlock(t *testing.T) {
	t.Parallel()

	t.Run("supported required", func(t *testing.T) {
		t.Parallel()
		if err := initPreflightLandlock("required", false, func() landlock.Report { return supportedLandlockReport() }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("too-old required", func(t *testing.T) {
		t.Parallel()
		err := initPreflightLandlock("required", false, func() landlock.Report { return tooOldLandlockReport() })
		if err == nil || !strings.Contains(err.Error(), "--landlock best-effort") {
			t.Fatalf("err = %v, want best-effort alternative", err)
		}
	})

	t.Run("unsupported required", func(t *testing.T) {
		t.Parallel()
		err := initPreflightLandlock("required", false, func() landlock.Report { return unsupportedLandlockReport() })
		if err == nil || !strings.Contains(err.Error(), "--landlock best-effort") {
			t.Fatalf("err = %v, want best-effort alternative", err)
		}
	})

	t.Run("explicit best-effort unsupported", func(t *testing.T) {
		t.Parallel()
		if err := initPreflightLandlock("best-effort", true, func() landlock.Report { return unsupportedLandlockReport() }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("explicit disabled unsupported", func(t *testing.T) {
		t.Parallel()
		if err := initPreflightLandlock("disabled", true, func() landlock.Report { return unsupportedLandlockReport() }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("implicit best-effort rejected", func(t *testing.T) {
		t.Parallel()
		err := initPreflightLandlock("best-effort", false, func() landlock.Report { return supportedLandlockReport() })
		if err == nil || !strings.Contains(err.Error(), "explicitly") {
			t.Fatalf("err = %v, want explicit", err)
		}
	})
}

func TestInitCmd(t *testing.T) {
	validServer := "http://127.0.0.1:8000"

	t.Run("help is side-effect-free", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"-h"})
		})
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(stderr, "mellomting init") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("bare init usage", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		code, _, stderr := captureOutput(t, func() int {
			return initCmd(nil)
		})
		if code != 2 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(stderr, "example: mellomting init --server http://127.0.0.1:8000") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("unknown flags rejected", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		for _, flag := range []string{"--backend", "--model", "--tls-cert", "--yes", "--allow-partial"} {
			code, _, _ := captureOutput(t, func() int {
				return initCmd([]string{validServer, flag})
			})
			if code != 2 {
				t.Fatalf("%s: code = %d", flag, code)
			}
		}
	})

	t.Run("dry-run succeeds without files", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		ts := fakeModelsServer(t, "alpha")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", ts.URL, "--config", cfg, "--dry-run"})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("entries = %v, want none", entries)
		}
	})

	t.Run("unsupported required fails without files", func(t *testing.T) {
		withInitLandlock(t, unsupportedLandlockReport())
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--config", cfg})
		})
		if code != 1 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(stderr, "--landlock best-effort") {
			t.Fatalf("stderr = %q", stderr)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("entries = %v, want none", entries)
		}
	})

	t.Run("explicit best-effort unsupported dry-run succeeds", func(t *testing.T) {
		withInitLandlock(t, unsupportedLandlockReport())
		ts := fakeModelsServer(t, "alpha")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", ts.URL, "--config", cfg, "--landlock", "best-effort", "--dry-run"})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("entries = %v, want none", entries)
		}
	})

	t.Run("duplicate last flag wins", func(t *testing.T) {
		withInitLandlock(t, unsupportedLandlockReport())
		ts := fakeModelsServer(t, "alpha")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", ts.URL, "--config", cfg, "--landlock", "required", "--landlock", "disabled", "--dry-run"})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
	})

	t.Run("invalid listener rejected", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--listen", "example.com:8080"})
		})
		if code != 2 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(stderr, "literal IP") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("duplicate canonical destination rejected", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--server", "http://[::ffff:127.0.0.1]:8000"})
		})
		if code != 2 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(stderr, "duplicate canonical destination") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("over maximum server count is a usage error", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		args := []string{}
		for i := 0; i <= discovery.MaxServers; i++ {
			args = append(args, "--server", fmt.Sprintf("s%d=http://127.0.0.1:%d", i, 9000+i))
		}
		code, _, stderr := captureOutput(t, func() int {
			return initCmd(args)
		})
		if code != 2 {
			t.Fatalf("over max: code = %d", code)
		}
		if !strings.Contains(stderr, "at most") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
}

func withInitPepper(t *testing.T, pepper []byte) {
	t.Helper()
	old := initPepperRand
	initPepperRand = func(int) ([]byte, error) { return pepper, nil }
	t.Cleanup(func() { initPepperRand = old })
}

func TestRenderInitArtifacts(t *testing.T) {
	deterministicPepper := make([]byte, 64)
	for i := range deterministicPepper {
		deterministicPepper[i] = byte(i)
	}

	t.Run("loopback config fixture", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"alpha": {"local"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}

		wantConfig := fmt.Sprintf(`auth:
    pepper_file: %s
    users_file: %s
models:
    alpha:
        servers:
            - local
        type: generation
security:
    backend_network:
        mode: loopback-only
    landlock:
        minimum_abi: %d
        mode: required
server:
    listen:
        address: 127.0.0.1:8080
        network: tcp
servers:
    local:
        url: http://127.0.0.1:8000
version: 1
`, args.PepperPath, args.UsersPath, landlock.DefaultMinimumABI)
		if string(got.Config) != wantConfig {
			t.Fatalf("config =\n%s\nwant\n%s", got.Config, wantConfig)
		}
		if string(got.Users) != auth.EmptyUsers {
			t.Fatalf("users = %q", got.Users)
		}
		wantPepper := base64.StdEncoding.EncodeToString(deterministicPepper) + "\n"
		if string(got.Pepper) != wantPepper {
			t.Fatalf("pepper = %q", got.Pepper)
		}
		if got.Normalized.Auth.UsersFile != args.UsersPath || got.Normalized.Auth.PepperFile != args.PepperPath {
			t.Fatalf("normalized auth = %+v", got.Normalized.Auth)
		}
		if got.Normalized.Security.Landlock.Mode != landlock.ModeRequired {
			t.Fatalf("landlock = %q", got.Normalized.Security.Landlock.Mode)
		}
		if got.Normalized.Security.BackendNetwork.Mode != "loopback-only" {
			t.Fatalf("backend network = %+v", got.Normalized.Security.BackendNetwork)
		}
		m := got.Normalized.Models["alpha"]
		if m.Type != "generation" || m.Strategy != "single" || len(m.Backends) != 1 {
			t.Fatalf("model = %+v", m)
		}

		again, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Config) != string(got.Config) {
			t.Fatalf("render not stable:\n%s\n%s", got.Config, again.Config)
		}
	})

	t.Run("multiple replicas use default strategy", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"a=http://127.0.0.1:8000", "b=http://127.0.0.1:8001"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"m": {"a", "b"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		m := got.Normalized.Models["m"]
		if m.Strategy != "least-inflight" || len(m.Backends) != 2 {
			t.Fatalf("model = %+v", m)
		}
		cfg := string(got.Config)
		ia := strings.Index(cfg, "- a\n")
		ib := strings.Index(cfg, "- b\n")
		if ia < 0 || ib < 0 || ia > ib {
			t.Fatalf("config =\n%s", cfg)
		}
	})

	t.Run("wildcard listener enables plaintext", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, "0.0.0.0:8080", "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"m": {"local"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Normalized.Server.AllowPlaintextNonLoopback {
			t.Fatalf("normalized server = %+v", got.Normalized.Server)
		}
		if !strings.Contains(string(got.Config), "allow_plaintext_non_loopback: true") {
			t.Fatalf("config =\n%s", got.Config)
		}
	})

	t.Run("unix listener uses default socket mode", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		sock := filepath.Join(dir, "mellomting.sock")
		args, err := parseInitArguments(cfgPath, sock, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"m": {"local"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		if got.Normalized.Server.Listen.Network != "unix" || got.Normalized.Server.Listen.Mode != config.DefaultUnixSocketMode {
			t.Fatalf("listen = %+v", got.Normalized.Server.Listen)
		}
		if !strings.Contains(string(got.Config), "mode: \"0660\"") {
			t.Fatalf("config =\n%s", got.Config)
		}
		if strings.Contains(string(got.Config), "allow_plaintext_non_loopback") {
			t.Fatalf("config =\n%s", got.Config)
		}
	})

	t.Run("mixed servers derive allowed-cidrs", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"a=http://192.168.1.20:8000", "b=http://127.0.0.1:8001"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"m": {"a", "b"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		if got.Normalized.Security.BackendNetwork.Mode != "allowed-cidrs" {
			t.Fatalf("backend network = %+v", got.Normalized.Security.BackendNetwork)
		}
		want := []string{"127.0.0.1/32", "192.168.1.20/32"}
		if !reflect.DeepEqual(got.Normalized.Security.BackendNetwork.CIDRs, want) {
			t.Fatalf("cidrs = %v, want %v", got.Normalized.Security.BackendNetwork.CIDRs, want)
		}
	})

	t.Run("selected landlock mode is rendered", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "best-effort", true, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{Models: map[string][]string{"m": {"local"}}}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		if got.Normalized.Security.Landlock.Mode != landlock.ModeBestEffort {
			t.Fatalf("landlock = %q", got.Normalized.Security.Landlock.Mode)
		}
		if !strings.Contains(string(got.Config), "mode: best-effort") {
			t.Fatalf("config =\n%s", got.Config)
		}
	})

	t.Run("empty discovery fails", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := renderInitArtifacts(args, discovery.Result{Models: map[string][]string{}}); err == nil || !strings.Contains(err.Error(), "no models") {
			t.Fatalf("err = %v, want no models", err)
		}
	})

	t.Run("pepper source receives 64 bytes", func(t *testing.T) {
		var got int
		old := initPepperRand
		initPepperRand = func(n int) ([]byte, error) {
			got = n
			return make([]byte, n), nil
		}
		t.Cleanup(func() { initPepperRand = old })

		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := renderInitArtifacts(args, discovery.Result{Models: map[string][]string{"m": {"local"}}}); err != nil {
			t.Fatal(err)
		}
		if got != initPepperBytes {
			t.Fatalf("pepper length = %d, want %d", got, initPepperBytes)
		}
	})

	t.Run("short pepper rejected", func(t *testing.T) {
		old := initPepperRand
		initPepperRand = func(int) ([]byte, error) { return make([]byte, 15), nil }
		t.Cleanup(func() { initPepperRand = old })

		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := renderInitArtifacts(args, discovery.Result{Models: map[string][]string{"m": {"local"}}}); err == nil || !strings.Contains(err.Error(), "pepper too short") {
			t.Fatalf("err = %v, want pepper too short", err)
		}
	})
}

func TestAuthInitByteParsers(t *testing.T) {
	t.Parallel()

	t.Run("empty users parses", func(t *testing.T) {
		t.Parallel()
		uf, err := auth.ParseUsers(auth.EmptyUsersBytes())
		if err != nil {
			t.Fatal(err)
		}
		if len(uf.Keys) != 0 {
			t.Fatalf("keys = %v", uf.Keys)
		}
	})

	t.Run("pepper validation", func(t *testing.T) {
		t.Parallel()
		if err := auth.ValidatePepper(make([]byte, 15)); err == nil || !strings.Contains(err.Error(), "pepper too short") {
			t.Fatalf("err = %v, want too short", err)
		}
		if err := auth.ValidatePepper(make([]byte, 16)); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

type scriptedCommitOps struct {
	inner           commitOps
	fail            map[string]error
	failOnce        map[string]bool
	after           map[string]func()
	calls           []string
	fstatParentFunc func(int) (commitFileIdentity, error)
}

func (s *scriptedCommitOps) failFor(keys ...string) error {
	for _, k := range keys {
		if err, ok := s.fail[k]; ok {
			return err
		}
		if s.failOnce[k] {
			s.failOnce[k] = false
			return fmt.Errorf("injected %s", k)
		}
	}
	return nil
}

func (s *scriptedCommitOps) run(keys []string, fn func() error) error {
	s.calls = append(s.calls, keys...)
	if err := s.failFor(keys...); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	for _, k := range keys {
		if hook := s.after[k]; hook != nil {
			hook()
		}
	}
	return nil
}

func (s *scriptedCommitOps) openParent(path string) (int, commitFileIdentity, error) {
	var (
		fd int
		id commitFileIdentity
	)
	err := s.run([]string{"openParent"}, func() error {
		var e error
		fd, id, e = s.inner.openParent(path)
		return e
	})
	return fd, id, err
}

func (s *scriptedCommitOps) fstatParent(fd int) (commitFileIdentity, error) {
	var id commitFileIdentity
	err := s.run([]string{"fstatParent"}, func() error {
		var e error
		if s.fstatParentFunc != nil {
			id, e = s.fstatParentFunc(fd)
		} else {
			id, e = s.inner.fstatParent(fd)
		}
		return e
	})
	return id, err
}

func (s *scriptedCommitOps) lstatInDir(fd int, name string) (commitFileIdentity, error) {
	var id commitFileIdentity
	err := s.run([]string{"lstatInDir", "lstatInDir:" + name}, func() error {
		var e error
		id, e = s.inner.lstatInDir(fd, name)
		return e
	})
	return id, err
}

func (s *scriptedCommitOps) createInDir(fd int, name string, mode uint32) (int, commitFileIdentity, error) {
	var (
		fileFD int
		id     commitFileIdentity
	)
	err := s.run([]string{"createInDir", "createInDir:" + name}, func() error {
		var e error
		fileFD, id, e = s.inner.createInDir(fd, name, mode)
		return e
	})
	return fileFD, id, err
}

func (s *scriptedCommitOps) writeAll(fd int, name string, data []byte) error {
	return s.run([]string{"writeAll", "writeAll:" + name}, func() error {
		return s.inner.writeAll(fd, name, data)
	})
}

func (s *scriptedCommitOps) fsyncFile(fd int, name string) error {
	return s.run([]string{"fsyncFile", "fsyncFile:" + name}, func() error {
		return s.inner.fsyncFile(fd, name)
	})
}

func (s *scriptedCommitOps) fstatFile(fd int, name string) (commitFileIdentity, error) {
	var id commitFileIdentity
	err := s.run([]string{"fstatFile", "fstatFile:" + name}, func() error {
		var e error
		id, e = s.inner.fstatFile(fd, name)
		return e
	})
	return id, err
}

func (s *scriptedCommitOps) closeFile(fd int, name string) error {
	return s.run([]string{"closeFile", "closeFile:" + name}, func() error {
		return s.inner.closeFile(fd, name)
	})
}

func (s *scriptedCommitOps) linkInDir(fd int, oldname, newname string) error {
	return s.run([]string{"linkInDir", "linkInDir:" + newname}, func() error {
		return s.inner.linkInDir(fd, oldname, newname)
	})
}

func (s *scriptedCommitOps) unlinkInDir(fd int, name string) error {
	return s.run([]string{"unlinkInDir", "unlinkInDir:" + name}, func() error {
		return s.inner.unlinkInDir(fd, name)
	})
}

func (s *scriptedCommitOps) fsyncDir(fd int) error {
	return s.run([]string{"fsyncDir"}, func() error {
		return s.inner.fsyncDir(fd)
	})
}

func commitTestArgs(t *testing.T, dir string, servers ...string) initArguments {
	t.Helper()
	args, err := parseInitArguments(filepath.Join(dir, "config.yaml"), defaultInitListen, "required", false, false, servers)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func commitTestArtifacts(t *testing.T, args initArguments, models map[string][]string) initArtifacts {
	t.Helper()
	pepper := make([]byte, 64)
	for i := range pepper {
		pepper[i] = byte(i + 1)
	}
	withInitPepper(t, pepper)
	arts, err := renderInitArtifacts(args, discovery.Result{Models: models})
	if err != nil {
		t.Fatal(err)
	}
	return arts
}

func newCommitDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dest")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory %s not empty: %v", dir, names)
	}
}

func TestCommitInitArtifacts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("init commit is Linux-only")
	}
	models := map[string][]string{"m": {"local"}}
	oneServer := []string{"http://127.0.0.1:8000"}

	t.Run("success and exact modes", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		if err := commitInitArtifacts(args, arts, nil); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{args.ConfigPath, args.UsersPath, args.PepperPath} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if st.Mode()&0o777 != 0o600 {
				t.Fatalf("%s mode = %o", p, st.Mode()&0o777)
			}
		}
		if b, _ := os.ReadFile(args.ConfigPath); !reflect.DeepEqual(b, arts.Config) {
			t.Fatalf("config bytes = %q", b)
		}
		if b, _ := os.ReadFile(args.UsersPath); !reflect.DeepEqual(b, arts.Users) {
			t.Fatalf("users bytes = %q", b)
		}
		if b, _ := os.ReadFile(args.PepperPath); !reflect.DeepEqual(b, arts.Pepper) {
			t.Fatalf("pepper bytes = %q", b)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 3 {
			t.Fatalf("entries = %v", entries)
		}
	})

	t.Run("parent mode and ownership", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o720, 0o775} {
			dir := newCommitDir(t)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			args := commitTestArgs(t, dir, oneServer...)
			arts := commitTestArtifacts(t, args, models)
			err := commitInitArtifacts(args, arts, nil)
			if err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
				t.Fatalf("mode %o: err = %v", mode, err)
			}
		}

		if os.Geteuid() == 0 {
			dir := newCommitDir(t)
			if err := os.Chown(dir, 12345, 12345); err != nil {
				t.Fatal(err)
			}
			args := commitTestArgs(t, dir, oneServer...)
			arts := commitTestArtifacts(t, args, models)
			err := commitInitArtifacts(args, arts, nil)
			if err == nil || !strings.Contains(err.Error(), "owned by the effective user") {
				t.Fatalf("err = %v", err)
			}
		}
	})

	t.Run("opened-parent identity revalidation", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		prod := defaultCommitOps()
		ops := &scriptedCommitOps{
			inner: prod,
			fstatParentFunc: func(fd int) (commitFileIdentity, error) {
				id, err := prod.fstatParent(fd)
				if err != nil {
					return commitFileIdentity{}, err
				}
				id.Ino++
				return id, nil
			},
		}
		err := commitInitArtifacts(args, arts, ops)
		if err == nil || !strings.Contains(err.Error(), "changed after it was opened") {
			t.Fatalf("err = %v", err)
		}
		requireDirEmpty(t, dir)
	})

	t.Run("existing destinations are refused", func(t *testing.T) {
		cases := []struct {
			name string
			kind string
		}{
			{"regular file", "file"},
			{"symlink", "symlink"},
			{"directory", "dir"},
			{"FIFO", "fifo"},
		}
		for _, tc := range cases {
			t.Run(tc.kind, func(t *testing.T) {
				dir := newCommitDir(t)
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
					p := filepath.Join(dir, name)
					switch tc.kind {
					case "file":
						if err := os.WriteFile(p, []byte("existing"), 0o600); err != nil {
							t.Fatal(err)
						}
					case "symlink":
						if err := os.Symlink(target, p); err != nil {
							t.Fatal(err)
						}
					case "dir":
						if err := os.Mkdir(p, 0o700); err != nil {
							t.Fatal(err)
						}
					case "fifo":
						if err := unix.Mknod(p, unix.S_IFIFO|0600, 0); err != nil {
							t.Fatal(err)
						}
					}
				}
				args := commitTestArgs(t, dir, oneServer...)
				arts := commitTestArtifacts(t, args, models)
				err := commitInitArtifacts(args, arts, nil)
				if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
					t.Fatalf("err = %v", err)
				}
				for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
					if !strings.Contains(err.Error(), filepath.Join(dir, name)) {
						t.Fatalf("err %q missing %q", err, filepath.Join(dir, name))
					}
				}
			})
		}
	})

	t.Run("failure injection leaves no files", func(t *testing.T) {
		keys := []string{
			"createInDir",
			"writeAll",
			"fsyncFile",
			"fstatFile",
			"closeFile",
			"linkInDir",
			"unlinkInDir",
			"fsyncDir",
		}
		for _, key := range keys {
			t.Run(key, func(t *testing.T) {
				dir := newCommitDir(t)
				args := commitTestArgs(t, dir, oneServer...)
				arts := commitTestArtifacts(t, args, models)
				ops := &scriptedCommitOps{inner: defaultCommitOps(), failOnce: map[string]bool{key: true}}
				err := commitInitArtifacts(args, arts, ops)
				if err == nil || !strings.Contains(err.Error(), "injected "+key) {
					t.Fatalf("err = %v", err)
				}
				requireDirEmpty(t, dir)
			})
		}
	})

	t.Run("auth sync boundary failures", func(t *testing.T) {
		t.Run("before directory sync", func(t *testing.T) {
			dir := newCommitDir(t)
			args := commitTestArgs(t, dir, oneServer...)
			arts := commitTestArtifacts(t, args, models)
			ops := &scriptedCommitOps{inner: defaultCommitOps(), failOnce: map[string]bool{"unlinkInDir": true}}
			err := commitInitArtifacts(args, arts, ops)
			if err == nil || !strings.Contains(err.Error(), "injected unlinkInDir") {
				t.Fatalf("err = %v", err)
			}
			requireDirEmpty(t, dir)
		})

		t.Run("after directory sync", func(t *testing.T) {
			dir := newCommitDir(t)
			args := commitTestArgs(t, dir, oneServer...)
			arts := commitTestArtifacts(t, args, models)
			ops := &scriptedCommitOps{inner: defaultCommitOps(), failOnce: map[string]bool{"linkInDir:config.yaml": true}}
			err := commitInitArtifacts(args, arts, ops)
			if err == nil || !strings.Contains(err.Error(), "injected linkInDir:config.yaml") {
				t.Fatalf("err = %v", err)
			}
			requireDirEmpty(t, dir)
		})
	})

	t.Run("destination race is not replaced", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		cfg := filepath.Join(dir, "config.yaml")
		ops := &scriptedCommitOps{
			inner: defaultCommitOps(),
			after: map[string]func(){
				"fsyncDir": func() {
					if _, err := os.Lstat(cfg); os.IsNotExist(err) {
						_ = os.WriteFile(cfg, []byte("attacker"), 0o600)
					}
				},
			},
		}
		err := commitInitArtifacts(args, arts, ops)
		if err == nil || !strings.Contains(err.Error(), "appeared before publication") {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(cfg); string(b) != "attacker" {
			t.Fatalf("config = %q", b)
		}
		if _, err := os.Lstat(args.UsersPath); !os.IsNotExist(err) {
			t.Fatalf("users should be rolled back: %v", err)
		}
		if _, err := os.Lstat(args.PepperPath); !os.IsNotExist(err) {
			t.Fatalf("pepper should be rolled back: %v", err)
		}
	})

	t.Run("temporary source replacement is detected", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		temp := fmt.Sprintf(".mellomting-init-%d-pepper.tmp", os.Getpid())
		tempPath := filepath.Join(dir, temp)
		replacement := filepath.Join(t.TempDir(), "replacement")
		if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		hooked := false
		ops := &scriptedCommitOps{
			inner: defaultCommitOps(),
			after: map[string]func(){
				"closeFile:" + temp: func() {
					hooked = true
					_ = os.Remove(tempPath)
					_ = os.Link(replacement, tempPath)
				},
			},
		}
		err := commitInitArtifacts(args, arts, ops)
		if err == nil || !strings.Contains(err.Error(), "changed after it was created") {
			t.Fatalf("err = %v hooked=%v calls=%v", err, hooked, ops.calls)
		}
		if !hooked {
			t.Fatalf("hook did not run")
		}
		if b, _ := os.ReadFile(tempPath); string(b) != "replacement" {
			t.Fatalf("temp = %q, want replacement preserved", b)
		}
		for _, name := range []string{"auth.pepper", "users.yaml", "config.yaml"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("%s should be absent: %v", name, err)
			}
		}
	})

	t.Run("parent rename race stays in the opened directory", func(t *testing.T) {
		dir := newCommitDir(t)
		renamed := dir + "-renamed"
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		ops := &scriptedCommitOps{
			inner: defaultCommitOps(),
			after: map[string]func(){
				"openParent": func() {
					_ = os.Rename(dir, renamed)
				},
			},
		}
		if err := commitInitArtifacts(args, arts, ops); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
			if _, err := os.Stat(filepath.Join(renamed, name)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("original directory should be renamed")
		}
	})

	t.Run("ancestor symlink race stays in the opened directory", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		other := filepath.Join(base, "other")
		realChild := filepath.Join(real, "child")
		otherChild := filepath.Join(other, "child")
		for _, d := range []string{real, other, realChild, otherChild} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		args, err := parseInitArguments(filepath.Join(link, "child", "config.yaml"), defaultInitListen, "required", false, false, oneServer)
		if err != nil {
			t.Fatal(err)
		}
		arts := commitTestArtifacts(t, args, models)
		ops := &scriptedCommitOps{
			inner: defaultCommitOps(),
			after: map[string]func(){
				"openParent": func() {
					_ = os.Remove(link)
					_ = os.Symlink(other, link)
				},
			},
		}
		if err := commitInitArtifacts(args, arts, ops); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.yaml", "users.yaml", "auth.pepper"} {
			if _, err := os.Stat(filepath.Join(realChild, name)); err != nil {
				t.Fatalf("real %s: %v", name, err)
			}
			if _, err := os.Lstat(filepath.Join(otherChild, name)); !os.IsNotExist(err) {
				t.Fatalf("other %s should be absent: %v", name, err)
			}
		}
	})

	t.Run("rollback removes only matching created inodes", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		pepper := filepath.Join(dir, "auth.pepper")
		pepperTemp := fmt.Sprintf(".mellomting-init-%d-pepper.tmp", os.Getpid())
		ops := &scriptedCommitOps{
			inner:    defaultCommitOps(),
			failOnce: map[string]bool{"unlinkInDir:" + pepperTemp: true},
			after: map[string]func(){
				"linkInDir:auth.pepper": func() {
					_ = os.Remove(pepper)
					_ = os.WriteFile(pepper, []byte("replacement"), 0o600)
				},
			},
		}
		err := commitInitArtifacts(args, arts, ops)
		if err == nil || !strings.Contains(err.Error(), "injected unlinkInDir:") {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(pepper); string(b) != "replacement" {
			t.Fatalf("pepper = %q, want replacement preserved", b)
		}
		if _, err := os.Lstat(filepath.Join(dir, "users.yaml")); !os.IsNotExist(err) {
			t.Fatalf("users should be absent")
		}
		if _, err := os.Lstat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
			t.Fatalf("config should be absent")
		}
	})

	t.Run("config is published last", func(t *testing.T) {
		dir := newCommitDir(t)
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		ops := &scriptedCommitOps{inner: defaultCommitOps()}
		if err := commitInitArtifacts(args, arts, ops); err != nil {
			t.Fatal(err)
		}
		idx := make(map[string]int)
		for i, c := range ops.calls {
			if _, ok := idx[c]; !ok {
				idx[c] = i
			}
		}
		for _, pair := range [][2]string{
			{"linkInDir:auth.pepper", "linkInDir:users.yaml"},
			{"linkInDir:users.yaml", "fsyncDir"},
			{"fsyncDir", "linkInDir:config.yaml"},
		} {
			if idx[pair[0]] > idx[pair[1]] {
				t.Fatalf("call order %v: %d > %d calls=%v", pair, idx[pair[0]], idx[pair[1]], ops.calls)
			}
		}
		configLink := idx["linkInDir:config.yaml"]
		configUnlink := -1
		finalFsync := -1
		for i := configLink + 1; i < len(ops.calls); i++ {
			c := ops.calls[i]
			if strings.HasPrefix(c, "unlinkInDir:.mellomting-init-") && configUnlink == -1 {
				configUnlink = i
			}
			if c == "fsyncDir" {
				finalFsync = i
			}
		}
		if configUnlink == -1 || finalFsync == -1 || configUnlink > finalFsync {
			t.Fatalf("calls = %v", ops.calls)
		}
	})

	t.Run("crash residue diagnostic", func(t *testing.T) {
		dir := newCommitDir(t)
		for _, name := range []string{"users.yaml", "auth.pepper"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("residue"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		args := commitTestArgs(t, dir, oneServer...)
		arts := commitTestArtifacts(t, args, models)
		err := commitInitArtifacts(args, arts, nil)
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.Error(), filepath.Join(dir, "users.yaml")) || !strings.Contains(err.Error(), filepath.Join(dir, "auth.pepper")) {
			t.Fatalf("err = %q", err)
		}
	})

}

func fakeModelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(fmt.Sprintf(`{"id":%q,"object":"model"}`, id))
		}
		b.WriteString(`]}`)
		_, _ = fmt.Fprint(w, b.String())
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestInitEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("init commit is Linux-only")
	}

	t.Run("two overlapping servers", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		ts1 := fakeModelsServer(t, "alpha", "beta")
		ts2 := fakeModelsServer(t, "beta", "gamma")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, stdout, stderr := captureOutput(t, func() int {
			return initCmd([]string{
				"--server", "a=" + ts1.URL,
				"--server", "b=" + ts2.URL,
				"--config", cfg,
			})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
		for _, want := range []string{
			"discovered 3 model(s):",
			"alpha: a",
			"beta: a, b",
			"gamma: b",
			"initialized " + cfg + " with " + filepath.Join(dir, "users.yaml") + " and " + filepath.Join(dir, "auth.pepper"),
			"mellomting key create --config " + cfg,
			"mellomting serve --config " + cfg,
		} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("stdout missing %q:\n%s", want, stdout)
			}
		}
		// The pepper bytes must never be printed.
		if pepper, err := os.ReadFile(filepath.Join(dir, "auth.pepper")); err == nil && len(pepper) > 0 {
			if strings.Contains(stdout, strings.TrimSuffix(string(pepper), "\n")) {
				t.Fatalf("stdout leaked pepper:\n%s", stdout)
			}
		}
		for _, p := range []string{cfg, filepath.Join(dir, "users.yaml"), filepath.Join(dir, "auth.pepper")} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if st.Mode()&0o777 != 0o600 {
				t.Fatalf("%s mode = %o", p, st.Mode()&0o777)
			}
		}
	})

	t.Run("dry-run emits config only and no files", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		ts := fakeModelsServer(t, "alpha")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, stdout, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", ts.URL, "--config", cfg, "--dry-run"})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
		if _, err := config.Parse([]byte(stdout)); err != nil {
			t.Fatalf("stdout is not parseable config: %v\n%s", err, stdout)
		}
		if strings.Contains(stdout, "initialized") || strings.Contains(stdout, "next:") || strings.Contains(stdout, "Writing") {
			t.Fatalf("dry-run stdout leaked completion/write claim:\n%s", stdout)
		}
		if !strings.Contains(stderr, "server \"local\": 1 model(s)") {
			t.Fatalf("stderr missing discovery context:\n%s", stderr)
		}
		requireDirEmpty(t, dir)
	})

	t.Run("broken stdout before publication writes nothing", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		ts := fakeModelsServer(t, "alpha")
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		oldStdout := os.Stdout
		os.Stdout = w
		code := initCmd([]string{"--server", ts.URL, "--config", cfg})
		os.Stdout = oldStdout
		_ = r.Close()
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		requireDirEmpty(t, dir)
	})

	t.Run("summary is bounded to 20 rows", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		ids := make([]string, 25)
		for i := range ids {
			ids[i] = fmt.Sprintf("model-%02d", i)
		}
		ts := fakeModelsServer(t, ids...)
		dir := newCommitDir(t)
		cfg := filepath.Join(dir, "config.yaml")
		code, stdout, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", ts.URL, "--config", cfg})
		})
		if code != 0 {
			t.Fatalf("code = %d stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, "(5 more omitted)") {
			t.Fatalf("stdout missing omitted count:\n%s", stdout)
		}
		if strings.Contains(stdout, "model-24: ") {
			t.Fatalf("stdout exceeded 20 rows:\n%s", stdout)
		}
	})
}
