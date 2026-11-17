package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"mellomting/internal/agent"
	"mellomting/internal/config"
	"mellomting/internal/logging"
)

// agentCmd runs `mellomting agent check`. The agent itself runs inside
// serve, from the configuration's attach section (PLAN §104).
//
// Exit codes: 0 ok, 1 failure, 2 usage error.
func agentCmd(args []string) int {
	if groupHelp(args, "agent", "  check      Check the attach section's key with the edge and probe its servers") {
		return 0
	}
	if len(args) > 0 && args[0] == "check" {
		return agentCheckCmd(args[1:])
	}
	fmt.Fprintln(os.Stderr, "mellomting: agent: the agent runs in serve, from the attach section of the configuration;\n"+
		"create one with: mellomting init --edge URL")
	return 2
}

// agentCheckCmd runs `mellomting agent check`: is the key accepted, and do
// the local servers answer? Nothing is attached.
func agentCheckCmd(args []string) int {
	fs := commandFlags("agent check", "Check the attach section's agent key with the edge (without attaching)\n"+
		"and probe its local servers. Exit status 0 when the edge accepts the key.")
	var configPath string
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: agent check: unexpected arguments %q\n", fs.Args())
		return 2
	}
	cfg, err := config.Load(ResolveConfigPath(configPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: agent check: invalid configuration: %v\n", err)
		return 1
	}
	if cfg.Attach == nil {
		fmt.Fprintln(os.Stderr, "mellomting: agent check: the configuration has no attach section")
		return 1
	}
	log, _ := logging.New(io.Discard, "error", "text")
	opts, err := attachOptions(cfg.Attach, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: agent check: %v\n", err)
		return 1
	}
	return checkAttach(opts, "agent check")
}

// checkAttach reports whether each local server answers yet (one still
// loading is fine: the agent waits for it), then checks the key with the
// edge.
func checkAttach(opts []agent.Options, cmd string) int {
	for _, o := range opts {
		if ids, err := agent.ProbeBackend(context.Background(), o); err == nil {
			fmt.Fprintf(os.Stdout, "server %s: %s\n", o.Backend.Redacted(), strings.Join(ids, ", "))
		} else {
			fmt.Fprintf(os.Stdout, "server %s: not answering yet (%v); the agent will wait for it\n", o.Backend.Redacted(), err)
		}
	}
	if err := agent.CheckEdge(context.Background(), opts[0]); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: %s: %v\n", cmd, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "agent key accepted by %s\n", opts[0].Edge.Redacted())
	return 0
}
