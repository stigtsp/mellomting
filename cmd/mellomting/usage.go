package main

import (
	"flag"
	"fmt"
	"os"

	"mellomting/internal/accounting"
	"mellomting/internal/config"
)

// usageCmd runs `mellomting usage report` (PLAN §44).
//
// Exit codes: 0 ok, 1 configuration or report error, 2 usage error.
func usageCmd(args []string) int {
	sub, rest := subcommand(args, "report")
	switch sub {
	case "report":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown usage subcommand %q (want report)\n", sub)
		return 2
	}

	fs := flag.NewFlagSet("mellomting usage "+sub, flag.ContinueOnError)
	var configPath string
	fs.StringVar(&configPath, "config", config.DefaultConfigPath, "configuration file path")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: usage %s: unexpected arguments %q\n", sub, fs.Args())
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: usage %s: %v\n", sub, err)
		return 1
	}
	if !cfg.Accounting.Enabled {
		fmt.Fprintln(os.Stderr, "mellomting: usage report: accounting is disabled in configuration")
		return 1
	}

	report, err := accounting.ReportFile(cfg.Accounting.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: usage report: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "%-16s %8s %12s %12s %12s %12s\n",
		"KEY_ID", "REQUESTS", "INPUT", "OUTPUT", "TOTAL", "CACHED")
	for _, k := range report.Keys {
		fmt.Fprintf(os.Stdout, "%-16s %8d %12d %12d %12d %12d\n",
			k.KeyID, k.Requests, k.InputTokens, k.OutputTokens, k.TotalTokens, k.CachedTokens)
	}
	return 0
}
