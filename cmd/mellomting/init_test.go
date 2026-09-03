package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
		tc := tc
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
		tc := tc
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
		tc := tc
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

	t.Run("valid preflight succeeds without files", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--config", cfg})
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

	t.Run("dry-run parses without files", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--config", cfg, "--dry-run"})
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
		dir := t.TempDir()
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

	t.Run("explicit best-effort unsupported succeeds without files", func(t *testing.T) {
		withInitLandlock(t, unsupportedLandlockReport())
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--config", cfg, "--landlock", "best-effort"})
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
		code, _, stderr := captureOutput(t, func() int {
			return initCmd([]string{"--server", validServer, "--landlock", "required", "--landlock", "disabled"})
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

	t.Run("maximum server count", func(t *testing.T) {
		withInitLandlock(t, supportedLandlockReport())
		args := []string{}
		for i := 0; i < discovery.MaxServers; i++ {
			args = append(args, "--server", fmt.Sprintf("s%d=http://127.0.0.1:%d", i, 9000+i))
		}
		code, _, stderr := captureOutput(t, func() int {
			return initCmd(args)
		})
		if code != 0 {
			t.Fatalf("max servers: code = %d stderr=%q", code, stderr)
		}

		args = append(args, "--server", fmt.Sprintf("s%d=http://127.0.0.1:%d", discovery.MaxServers, 9000+discovery.MaxServers))
		code, _, stderr = captureOutput(t, func() int {
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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

		dir := t.TempDir()
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

		dir := t.TempDir()
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
