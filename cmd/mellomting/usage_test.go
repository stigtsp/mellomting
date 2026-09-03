package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mellomting/internal/accounting"
)

// usageFixture writes a config with accounting enabled plus a usage JSONL
// file with known records, and returns the binary, config path, and usage
// path.
func usageFixture(t *testing.T) (bin, cfgPath, usagePath string) {
	t.Helper()
	bin = buildCLI(t)
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	usagePath = filepath.Join(dir, "usage.jsonl")

	cfg := `version: 1

server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"

accounting:
  enabled: true
  path: ` + usagePath + `

servers:
  qwen-a:
    url: http://127.0.0.1:8001

models:
  qwen-coder:
    type: generation
    strategy: single
    upstream_model: Qwen/Qwen3-Coder
    servers:
      - qwen-a
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, cfgPath, usagePath
}

// writeUsageLine appends one accounting record to the usage JSONL file.
func writeUsageLine(t *testing.T, path string, r accounting.Record) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageReport(t *testing.T) {
	bin, cfgPath, usagePath := usageFixture(t)

	writeUsageLine(t, usagePath, accounting.Record{KeyID: "key-a", InputTokens: 10, OutputTokens: 5, TotalTokens: 15})
	writeUsageLine(t, usagePath, accounting.Record{KeyID: "key-a", InputTokens: 20, OutputTokens: 10, TotalTokens: 30})
	writeUsageLine(t, usagePath, accounting.Record{KeyID: "key-b", InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	// A malformed line must be skipped, not fatal.
	f, _ := os.OpenFile(usagePath, os.O_APPEND|os.O_WRONLY, 0o640)
	_, _ = f.WriteString("{not json}\n")
	_ = f.Close()

	code, out, errOut := runCLI(t, bin, "", "usage", "report", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("usage report exit = %d stderr=%q", code, errOut)
	}
	// Header present.
	if !strings.Contains(out, "KEY_ID") || !strings.Contains(out, "REQUESTS") {
		t.Fatalf("missing header: %q", out)
	}
	// key-a: 2 requests, 45 total tokens.
	if !strings.Contains(out, "key-a") || !strings.Contains(out, "45") {
		t.Fatalf("key-a totals missing: %q", out)
	}
	// key-b: 1 request, 2 total tokens.
	if !strings.Contains(out, "key-b") || !strings.Contains(out, "2") {
		t.Fatalf("key-b totals missing: %q", out)
	}
}

func TestUsageReportAccountingDisabled(t *testing.T) {
	bin, dir := keyCLIFixture(t)
	cfgPath := filepath.Join(dir, "config.yaml")

	code, _, errOut := runCLI(t, bin, "", "usage", "report", "-config", cfgPath)
	if code != 1 {
		t.Fatalf("exit = %d (want 1 when accounting disabled)", code)
	}
	if !strings.Contains(errOut, "accounting is disabled") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestUsageReportMissingFileIsEmpty(t *testing.T) {
	bin, cfgPath, _ := usageFixture(t)
	// No usage file written: an empty report is not an error.
	code, out, errOut := runCLI(t, bin, "", "usage", "report", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "KEY_ID") {
		t.Fatalf("header missing: %q", out)
	}
}

func TestUsageUnknownSubcommand(t *testing.T) {
	bin, cfgPath, _ := usageFixture(t)
	if code, _, _ := runCLI(t, bin, "", "usage", "bogus", "-config", cfgPath); code != 2 {
		t.Fatalf("exit = %d (want 2)", code)
	}
}
