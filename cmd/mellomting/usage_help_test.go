package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestUsageHelpHasNoPlanReferences: the help text is operator-facing
// output on the target host, which has no repository; PLAN citations
// belong in the repo docs, not in CLI help.
func TestUsageHelpHasNoPlanReferences(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	if strings.Contains(buf.String(), "PLAN") {
		t.Errorf("usage help references PLAN:\n%s", buf.String())
	}
}

func TestCommandHelp(t *testing.T) {
	commands := []string{
		"", "init", "serve", "install", "key", "key create", "key list",
		"key enable", "key disable", "key revoke", "config", "config check",
		"config show-effective", "sandbox", "sandbox check", "usage", "usage report",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			args := append([]string{"mellomting"}, strings.Fields(command)...)
			args = append(args, "--help")
			code, out, errOut := captureOutput(t, func() int { return commandDispatch(args) })
			if code != 0 || errOut != "" || !strings.Contains(out, "Usage: mellomting") {
				t.Fatalf("help: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if command == "key" && (!strings.Contains(out, "create") || !strings.Contains(out, "revoke")) {
				t.Fatalf("key help must list its commands: %q", out)
			}
			if command == "serve" && !strings.Contains(out, "--config PATH") {
				t.Fatalf("flag spelling missing: %q", out)
			}
		})
	}
}

func TestInvalidFlagsRemainErrors(t *testing.T) {
	for _, command := range []string{"init", "serve", "install", "key create", "config check", "sandbox check", "usage report"} {
		t.Run(command, func(t *testing.T) {
			args := append([]string{"mellomting"}, strings.Fields(command)...)
			args = append(args, "--unknown-flag")
			code, out, errOut := captureOutput(t, func() int { return commandDispatch(args) })
			if code != 2 || out != "" || !strings.Contains(errOut, "flag provided but not defined") {
				t.Fatalf("invalid flag: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

func TestInitNextStepsModelSelection(t *testing.T) {
	for _, count := range []int{1, 2} {
		var out bytes.Buffer
		if err := printInitCompletion(&out, initArguments{ConfigPath: "/tmp/config.yaml"}, count); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "--models MODEL") != (count > 1) {
			t.Fatalf("%d models: %s", count, out.String())
		}
		if !strings.Contains(out.String(), "--name local") {
			t.Fatalf("next command must use a valid username: %s", out.String())
		}
	}
}
