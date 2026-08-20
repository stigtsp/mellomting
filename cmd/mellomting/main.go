package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/landlock"
	"mellomting/internal/version"
)

// plannedCommands exist in the CLI but are implemented in later phases
// (PLAN §7, §91-98).
var plannedCommands = map[string]bool{
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
	case "key":
		os.Exit(keyCmd(os.Args[2:]))
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
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

// keyCmd runs the offline key-management subcommands (PLAN §29). The daemon
// treats the users file as read-only; only this CLI rewrites it.
//
// Exit codes: 0 ok, 1 load/validation failure or unknown id, 2 usage error.
func keyCmd(args []string) int {
	sub, rest := subcommand(args, "list")
	switch sub {
	case "create", "list", "enable", "disable", "revoke":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown key subcommand %q (want create, list, enable, disable, or revoke)\n", sub)
		return 2
	}

	c, code := keyParseFlags(sub, rest)
	if code != 0 {
		return code
	}

	switch sub {
	case "create":
		return keyCreate(c)
	case "list":
		return keyList(c)
	case "enable", "disable":
		return keySetEnabled(c, sub == "enable")
	case "revoke":
		return keyRevoke(c)
	}
	return 2
}

type keyFlags struct {
	configPath string
	name       string
	models     string
	expires    string
	id         string
}

func keyParseFlags(sub string, rest []string) (*keyFlags, int) {
	fs := flag.NewFlagSet("mellomting key "+sub, flag.ContinueOnError)
	c := &keyFlags{}
	fs.StringVar(&c.configPath, "config", config.DefaultConfigPath, "configuration file path")
	if sub == "create" {
		fs.StringVar(&c.name, "name", "", "human-readable key name (required)")
		fs.StringVar(&c.models, "models", "", "comma-separated model names, or * (required)")
		fs.StringVar(&c.expires, "expires", "", "expiry as RFC3339 (optional)")
	}
	if sub == "enable" || sub == "disable" || sub == "revoke" {
		fs.StringVar(&c.id, "id", "", "key id (required)")
	}
	if err := fs.Parse(rest); err != nil {
		return nil, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: %s: unexpected arguments %q\n", sub, fs.Args())
		return nil, 2
	}

	switch sub {
	case "create":
		if c.name == "" {
			fmt.Fprintln(os.Stderr, "mellomting: key create: --name is required")
			return nil, 2
		}
		if c.models == "" {
			fmt.Fprintln(os.Stderr, "mellomting: key create: --models is required")
			return nil, 2
		}
		if c.expires != "" {
			if _, err := time.Parse(time.RFC3339, c.expires); err != nil {
				fmt.Fprintf(os.Stderr, "mellomting: key create: invalid --expires %q (want RFC3339)\n", c.expires)
				return nil, 2
			}
		}
	case "list":
		// no arguments
	case "enable", "disable", "revoke":
		if c.id == "" {
			fmt.Fprintf(os.Stderr, "mellomting: key %s: --id is required\n", sub)
			return nil, 2
		}
	}
	return c, 0
}

// keyState loads configuration and resolves the users-file and pepper paths.
func keyState(c *keyFlags) (cfg *config.Config, usersPath, pepperPath string, exit int) {
	cfg, err := config.Load(c.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key: load config: %v\n", err)
		return nil, "", "", 1
	}
	usersPath = cfg.Auth.UsersFile
	pepperPath = cfg.Auth.PepperFile
	if usersPath == "" {
		fmt.Fprintln(os.Stderr, "mellomting: key: auth.users_file is not set in configuration")
		return nil, "", "", 1
	}
	return cfg, usersPath, pepperPath, 0
}

func keyCreate(c *keyFlags) int {
	_, usersPath, pepperPath, exit := keyState(c)
	if exit != 0 {
		return exit
	}

	pepper, err := auth.LoadPepper(pepperPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return 1
	}
	key, id, err := auth.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return 1
	}

	models := splitModels(c.models)
	var exp *time.Time
	if c.expires != "" {
		t, err := time.Parse(time.RFC3339, c.expires)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
			return 1
		}
		exp = &t
	}

	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID:         id,
			Name:       c.name,
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true,
			ExpiresAt:  exp,
			Models:     models,
		})
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: write users file: %v\n", err)
		return 1
	}

	// The raw secret is printed exactly once (PLAN §29).
	fmt.Fprintf(os.Stdout, "created key:          %s\n", key)
	fmt.Printf("  id:                %s\n", id)
	fmt.Printf("  name:              %s\n", c.name)
	fmt.Printf("  models:            %s\n", strings.Join(models, ", "))
	if exp != nil {
		fmt.Printf("  expires:           %s\n", exp.Format(time.RFC3339))
	}
	fmt.Printf("  users file:        %s\n", usersPath)
	fmt.Fprintln(os.Stdout, "Store the secret now; it is not retrievable later.")
	return 0
}

func keySetEnabled(c *keyFlags, enable bool) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	verb := "enabled"
	if !enable {
		verb = "disabled"
	}
	err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		return setEnabled(uf, c.id, enable)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key %s: %v\n", verb, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "%s key %s\n", verb, c.id)
	return 0
}

func keyList(c *keyFlags) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	uf, err := auth.LoadUsers(usersPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key list: %v\n", err)
		return 1
	}
	if len(uf.Keys) == 0 {
		fmt.Println("no keys")
		return 0
	}
	fmt.Printf("%-10s %-16s %-10s %-22s %s\n", "ID", "NAME", "STATUS", "EXPIRES", "MODELS")
	for i := range uf.Keys {
		k := &uf.Keys[i]
		status := "enabled"
		if !k.Enabled {
			status = "disabled"
		}
		expirs := "-"
		if k.ExpiresAt != nil {
			expirs = k.ExpiresAt.Format(time.RFC3339)
		}
		fmt.Printf("%-10s %-16s %-10s %-22s %s\n", k.ID, k.Name, status, expirs, strings.Join(k.Models, ", "))
	}
	return 0
}

func keyRevoke(c *keyFlags) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		return revoke(uf, c.id)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "revoked key %s\n", c.id)
	return 0
}

func setEnabled(uf *auth.UsersFile, id string, enable bool) error {
	for i := range uf.Keys {
		if uf.Keys[i].ID == id {
			uf.Keys[i].Enabled = enable
			return nil
		}
	}
	return fmt.Errorf("key id %q not found", id)
}

func revoke(uf *auth.UsersFile, id string) error {
	for i := range uf.Keys {
		if uf.Keys[i].ID == id {
			uf.Keys = append(uf.Keys[:i], uf.Keys[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("key id %q not found", id)
}

func splitModels(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "*" {
			return []string{"*"}
		}
		out = append(out, p)
	}
	return out
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: mellomting <command> [flags]

commands:
  version                      print version information
  help                         print this help
  config check                 validate configuration (exit 0 when valid)
  config show-effective        print the effective configuration with defaults
  sandbox check                report Landlock capability and policy result
  key <subcommand>             offline API key management

key subcommands (docs/PLAN.md §29):
  key create   --name NAME --models M[,M...] [--expires RFC3339]
               create a key and print it once
  key list                            list keys (id, name, models, status)
  key enable  --id ID                 re-enable a key
  key disable --id ID                 disable a key
  key revoke  --id ID                 permanently remove a key
  all key subcommands accept -config PATH (default: mellomting config path)

  serve -config PATH           run the proxy daemon (default: config path)

planned commands (docs/PLAN.md):
  usage                        report token/request usage
`)
}
