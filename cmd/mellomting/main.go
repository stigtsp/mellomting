package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"mellomting/internal/config"
	"mellomting/internal/landlock"
	"mellomting/internal/version"
)

// plannedCommands exist in the CLI but are implemented in later phases
// (PLAN §7, §91-98).
var plannedCommands = map[string]bool{
	"serve": true,
	"key":   true,
	"usage": true,
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Fprintln(os.Stdout, version.String())
	case "help", "-h", "--help":
		usage(os.Stdout)
	case "config":
		os.Exit(configCmd(os.Args[2:]))
	case "sandbox":
		os.Exit(sandboxCmd(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "mellomting: %s\n", describeUnknownCommand(os.Args[1]))
		usage(os.Stderr)
		os.Exit(2)
	}
}

func describeUnknownCommand(cmd string) string {
	if plannedCommands[cmd] {
		return fmt.Sprintf("command %q is not implemented yet (see docs/PLAN.md)", cmd)
	}
	return fmt.Sprintf("unknown command %q", cmd)
}

func subcommand(args []string, defaults string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return defaults, args
}

// configCmd runs `mellomting config [check|show-effective]`.
//
// Exit codes: 0 ok, 1 invalid or unreadable configuration, 2 usage error.
func configCmd(args []string) int {
	sub, rest := subcommand(args, "check")
	switch sub {
	case "check", "show-effective":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown config subcommand %q (want check or show-effective)\n", sub)
		return 2
	}

	fs := flag.NewFlagSet("mellomting config "+sub, flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultConfigPath, "configuration file path")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: config %s failed: %v\n", sub, err)
		return 1
	}

	switch sub {
	case "check":
		fmt.Fprintf(os.Stdout, "mellomting: configuration at %s is valid\n", *configPath)
	case "show-effective":
		out, err := yaml.Marshal(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: show-effective failed: %v\n", err)
			return 1
		}
		fmt.Fprint(os.Stdout, string(out))
	}
	return 0
}

// sandboxCmd runs `mellomting sandbox check`.
//
// Without -config the command is a pure capability report and exits 0.
// With -config the configured security.landlock policy is enforced in the
// result: mode required plus an unsupported or too-old kernel exits 1.
func sandboxCmd(args []string) int {
	sub, rest := subcommand(args, "check")
	if sub != "check" {
		fmt.Fprintf(os.Stderr, "mellomting: unknown sandbox subcommand %q (want check)\n", sub)
		return 2
	}

	fs := flag.NewFlagSet("mellomting sandbox check", flag.ContinueOnError)
	configPath := fs.String("config", "", "configuration file for landlock mode/minimum_abi (optional)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	report := landlock.Check()
	mode := landlock.ModeRequired
	minABI := landlock.DefaultMinimumABI
	enforce := false
	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: sandbox check failed: %v\n", err)
			return 1
		}
		mode = cfg.Security.Landlock.Mode
		minABI = cfg.Security.Landlock.MinimumABI
		enforce = true
	}

	fmt.Println("mellomting: sandbox check")
	fmt.Printf("  platform:          %s\n", report.Platform)
	fmt.Printf("  library max ABI:   %d\n", landlock.MaxABI)

	ok := false
	if report.Supported {
		fmt.Printf("  kernel ABI:        %d\n", report.KernelABI)
		ok = report.KernelABI >= minABI
	} else {
		fmt.Printf("  landlock:          unavailable (%s)\n", report.Reason)
	}
	fmt.Printf("  mode:              %s\n", mode)
	fmt.Printf("  minimum required:  %d\n", minABI)

	if ok {
		abi := min(report.KernelABI, landlock.MaxABI)
		fmt.Printf("  result:            ok (will enforce Landlock ABI %d)\n", abi)
		return 0
	}

	if !enforce {
		capability := report.Reason
		if report.Supported {
			capability = fmt.Sprintf("kernel ABI %d below default minimum %d", report.KernelABI, minABI)
		}
		fmt.Printf("  result:            report only (Landlock: %s)\n", capability)
		return 0
	}

	if mode != landlock.ModeRequired {
		fmt.Printf("  result:            not enforced (mode %s)\n", mode)
		return 0
	}

	if report.Supported {
		fmt.Printf("  result:            FAIL (kernel ABI %d below required minimum %d)\n", report.KernelABI, minABI)
	} else {
		fmt.Printf("  result:            FAIL (Landlock required but unavailable: %s)\n", report.Reason)
	}
	return 1
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: mellomting <command> [flags]

commands:
  version                      print version information
  help                         print this help
  config check                 validate configuration (exit 0 when valid)
  config show-effective        print the effective configuration with defaults
  sandbox check                report Landlock capability and policy result
  version/help flags: -config / -h where applicable

planned commands (docs/PLAN.md):
  serve                        run the proxy daemon
  key create|list|disable|enable|revoke
                               offline API key management
  usage                        report token/request usage
`)
}
