package main

import (
	"bytes"
	"strings"
	"testing"

	"mellomting/internal/discovery"
	"mellomting/internal/systemd"
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
		"", "init", "serve", "install", "top", "key", "key create", "key list",
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

// TestInitSummaryWording pins the discovery summary on every platform.
// The end-to-end journey that showed this text only runs where init can
// write files, so its wording went stale unnoticed until CI.
func TestInitSummaryWording(t *testing.T) {
	var out bytes.Buffer
	if err := printInitSummary(&out, discovery.Result{Models: map[string][]string{
		"alpha": {"a"},
		"beta":  {"a", "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"discovered 2 models:", "  alpha: a\n", "  beta: a, b\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := printInitSummary(&out, discovery.Result{Models: map[string][]string{"alpha": {"a"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "discovered 1 model:") {
		t.Fatalf("one model must not be pluralised:\n%s", out.String())
	}
}

func TestInitNextSteps(t *testing.T) {
	var out bytes.Buffer
	if err := printInitCompletion(&out, initArguments{ConfigPath: "/tmp/config.yaml"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "key create local --config /tmp/config.yaml") {
		t.Fatalf("next command must create a key with a valid username: %s", out.String())
	}
}

// TestInitNextStepsSystemConfig: an init into the system configuration
// path (a package or `install --systemd` deployment) is run by the
// packaged unit, so the next steps name systemctl, not `serve`, and
// need no --config because that path is the default.
func TestInitNextStepsSystemConfig(t *testing.T) {
	var out bytes.Buffer
	if err := printInitCompletion(&out, initArguments{ConfigPath: systemd.ConfigPath}); err != nil {
		t.Fatal(err)
	}
	want := "initialized /etc/mellomting/config.yaml\nnext:\n  sudo mellomting key create local\n  sudo systemctl enable --now mellomting\n"
	if out.String() != want {
		t.Fatalf("next steps for the system config path:\n%s\nwant:\n%s", out.String(), want)
	}
}
