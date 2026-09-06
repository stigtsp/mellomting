package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/landlock"
	"mellomting/internal/version"
)

func main() {
	os.Exit(commandDispatch(os.Args))
}

// commandDispatch dispatches the top-level command and returns the process
// exit code. It is factored out of main so the dispatch is testable without
// os.Exit.
func commandDispatch(argv []string) int {
	if len(argv) < 2 {
		usage(os.Stderr)
		return 2
	}

	switch argv[1] {
	case "version", "-v", "--version":
		fmt.Fprintln(os.Stdout, version.String())
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case "install":
		return installCmd(argv[2:])
	case "init":
		return initCmd(argv[2:])
	case "config":
		return configCmd(argv[2:])
	case "sandbox":
		return sandboxCmd(argv[2:])
	case "key":
		return keyCmd(argv[2:])
	case "usage":
		return usageCmd(argv[2:])
	case "serve":
		return serveCmd(argv[2:])
	default:
		fmt.Fprintf(os.Stderr, "mellomting: %s\n", describeUnknownCommand(argv[1]))
		usage(os.Stderr)
		return 2
	}
}

func describeUnknownCommand(cmd string) string {
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
	if groupHelp(args, "config", "  check           Validate configuration\n  show-effective  Show configuration with defaults") {
		return 0
	}
	sub, rest := subcommand(args, "check")
	switch sub {
	case "check", "show-effective":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown config subcommand %q (want check or show-effective)\n", sub)
		return 2
	}

	summary := "Validate configuration."
	if sub == "show-effective" {
		summary = "Show configuration with defaults."
	}
	fs := commandFlags("config "+sub, summary)
	var configPath string
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	if err := parseCommandFlags(fs, rest); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: config %s: unexpected arguments %q\n", sub, fs.Args())
		return 2
	}
	resolved := ResolveConfigPath(configPath)

	if sub == "check" {
		if _, err := config.Load(resolved); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: config %s failed: %v\n", sub, err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "Configuration at %s is valid.\n", resolved)
		return 0
	}

	_, out, err := config.LoadEffective(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: config %s failed: %v\n", sub, err)
		return 1
	}
	fmt.Fprint(os.Stdout, string(out))
	return 0
}

// sandboxCmd runs `mellomting sandbox check`.
//
// Without -config the command is a pure capability report and exits 0.
// With -config the configured security.landlock policy is enforced in the
// result: mode required plus an unsupported or too-old kernel exits 1.
func sandboxCmd(args []string) int {
	if groupHelp(args, "sandbox", "  check  Check Landlock support and configured policy") {
		return 0
	}
	sub, rest := subcommand(args, "check")
	if sub != "check" {
		fmt.Fprintf(os.Stderr, "mellomting: unknown sandbox subcommand %q (want check)\n", sub)
		return 2
	}

	fs := commandFlags("sandbox check", "Check Landlock support and configured policy. Without a readable default config, report capability only.")
	var configPath string
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	if err := parseCommandFlags(fs, rest); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: sandbox %s: unexpected arguments %q\n", sub, fs.Args())
		return 2
	}

	resolved := ResolveConfigPath(configPath)

	report := landlock.Check()
	mode := landlock.ModeRequired
	minABI := landlock.DefaultMinimumABI
	enforce := false
	if cfg, loadErr := config.Load(resolved); loadErr == nil {
		mode = cfg.Security.Landlock.Mode
		minABI = cfg.Security.Landlock.MinimumABI
		enforce = true
	} else if configPath != "" {
		// An explicit --config must be loadable; report the failure rather
		// than silently running report-only. An absent or unreadable system
		// default (/etc) is allowed to fall back to report-only so the check
		// works without a configuration.
		fmt.Fprintf(os.Stderr, "mellomting: sandbox check failed: %v\n", loadErr)
		return 1
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
//	mellomting key create NAME [flags]
//	mellomting key list
//	mellomting key enable|disable|revoke ID
//
// Exit codes: 0 ok, 1 load/validation failure or unknown id, 2 usage error.
func keyCmd(args []string) int {
	if groupHelp(args, "key", "  create NAME  Create an API key\n  list         List keys\n  enable ID    Enable a key\n  disable ID   Disable a key\n  revoke ID    Permanently remove a key") {
		return 0
	}
	sub, rest := subcommand(args, "list")
	switch sub {
	case "create", "list", "enable", "disable", "revoke":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown key subcommand %q (want create, list, enable, disable, or revoke)\n", sub)
		return 2
	}

	c, code := keyParseFlags(sub, rest)
	if c == nil {
		return code
	}

	switch sub {
	case "create":
		return keyCreate(c)
	case "list":
		return keyList(c)
	case "enable":
		return keySetEnabled(c, sub, true)
	case "disable":
		return keySetEnabled(c, sub, false)
	default:
		return keyRevoke(c)
	}
}

// keyFlags is one parsed key subcommand. operand is the NAME of a key to
// create or the ID of a key to enable, disable, or revoke.
type keyFlags struct {
	configPath         string
	operand            string
	models             string
	expires            *time.Time
	concurrentRequests int
	requestsPerSecond  float64
	burst              int
}

// keyOperand names the positional argument of each subcommand; list
// takes none.
var keyOperand = map[string]string{"create": "NAME", "enable": "ID", "disable": "ID", "revoke": "ID"}

func keyParseFlags(sub string, rest []string) (*keyFlags, int) {
	var summary string
	switch sub {
	case "create":
		summary = "Create an API key for NAME. The key is printed once, to stdout.\nIt may use every model unless --models narrows it."
	case "list":
		summary = "List keys and their status."
	case "enable":
		summary = "Enable a key."
	case "disable":
		summary = "Disable a key."
	case "revoke":
		summary = "Permanently remove a key."
	}
	operand := keyOperand[sub]
	fs := commandFlags(strings.TrimSpace("key "+sub+" "+operand), summary)
	c := &keyFlags{}
	fs.StringVar(&c.configPath, "config", "", configFlagHelp)
	var expires string
	if sub == "create" {
		fs.StringVar(&c.models, "models", config.ModelWildcard, "comma-separated `MODELS` the key may use; * means all")
		fs.StringVar(&expires, "expires", "", "expiry `DATE`: YYYY-MM-DD or RFC3339")
		fs.IntVar(&c.concurrentRequests, "concurrent-requests", 0, "maximum concurrent requests (0: use default)")
		fs.Float64Var(&c.requestsPerSecond, "requests-per-second", 0, "request rate limit (0: no rate limit)")
		fs.IntVar(&c.burst, "burst", 0, "rate-limit burst (defaults to one second of rate)")
	}

	// The operand may sit before or after the flags.
	if operand != "" {
		c.operand, rest = subcommand(rest, "")
	}
	if err := parseCommandFlags(fs, rest); err != nil {
		return nil, flagExitCode(err)
	}
	if c.operand == "" && operand != "" && fs.NArg() == 1 {
		c.operand, rest = fs.Arg(0), nil
	} else if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: key %s: unexpected arguments %q\n", sub, fs.Args())
		return nil, 2
	}
	if operand != "" && c.operand == "" {
		fmt.Fprintf(os.Stderr, "mellomting: key %s: missing %s (usage: mellomting key %s %s [flags])\n", sub, operand, sub, operand)
		return nil, 2
	}
	if sub != "create" {
		return c, 0
	}

	// D18: NAME is the key's username segment; validate it before any
	// entropy use or filesystem mutation.
	if err := auth.ValidateUsername(c.operand); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return nil, 2
	}
	if expires != "" {
		t, err := parseExpiry(expires)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: key create: invalid --expires %q (want YYYY-MM-DD or RFC3339)\n", expires)
			return nil, 2
		}
		c.expires = &t
	}
	// Per-key limits fail closed on negative values (T-X8); the daemon
	// rejects them at the key-store boundary, the CLI must not emit them
	// in the first place.
	for _, l := range []struct {
		flag  string
		value float64
	}{
		{"concurrent-requests", float64(c.concurrentRequests)},
		{"requests-per-second", c.requestsPerSecond},
		{"burst", float64(c.burst)},
	} {
		if l.value < 0 {
			fmt.Fprintf(os.Stderr, "mellomting: key create: --%s must be >= 0, got %v\n", l.flag, l.value)
			return nil, 2
		}
	}
	return c, 0
}

// parseExpiry accepts a bare date (expiring at the start of that day,
// UTC) or a full RFC 3339 timestamp.
func parseExpiry(s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// keyState loads configuration and resolves the users-file and pepper paths.
func keyState(c *keyFlags) (cfg *config.Config, usersPath, pepperPath string, exit int) {
	resolved := ResolveConfigPath(c.configPath)
	cfg, err := config.Load(resolved)
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
	cfg, usersPath, pepperPath, exit := keyState(c)
	if exit != 0 {
		return exit
	}

	pepper, err := auth.LoadPepper(pepperPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return 1
	}

	models, err := splitModels(c.models)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return 2
	}
	// A model not present in the loaded config is legal (an ACL may name
	// a model about to be added), but the operator should hear about it
	// (T-L4).
	for _, m := range models {
		if _, ok := cfg.Models[m]; !ok && m != config.ModelWildcard {
			fmt.Fprintf(os.Stderr, "mellomting: key create: warning: model %q is not present in the configuration; the ACL will allow it once the model exists\n", m)
		}
	}
	limits := auth.KeyLimits{
		ConcurrentRequests: c.concurrentRequests,
		RequestsPerSecond:  c.requestsPerSecond,
		Burst:              c.burst,
	}

	// D18: generate the key inside the exclusive update, retrying a key-ID
	// collision against the loaded users file at most eight times before
	// failing without mutation.
	var key, id string
	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		var genErr error
		key, id, genErr = chooseKeyID(func() (string, string, error) {
			return auth.Generate(c.operand)
		}, uf, 8)
		if genErr != nil {
			return genErr
		}
		uf.Keys = append(uf.Keys, auth.Key{
			ID:         id,
			Name:       c.operand,
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true,
			ExpiresAt:  c.expires,
			Models:     models,
			Limits:     limits,
		})
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: write users file: %v\n", err)
		return 1
	}

	// D14/D18: stdout carries the raw key and a trailing newline only, so a
	// script can capture it directly (KEY=$(mellomting key create ...)). All
	// human context goes to stderr: one ACL-confirmation line plus one apply
	// instruction (PLAN §10, D16). The key is shown exactly once.
	fmt.Fprintln(os.Stdout, key)
	access := strings.Join(models, ", ")
	if access == config.ModelWildcard {
		access = "all models"
	}
	fmt.Fprintf(os.Stderr, "Created API key %q for %s.\n", c.operand, access)
	fmt.Fprintln(os.Stderr, keyApplyInstruction)
	return 0
}

func keySetEnabled(c *keyFlags, sub string, enable bool) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		return setEnabled(uf, c.operand, enable)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key %s: %v\n", sub, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "%sd key %s\n", sub, c.operand)
	fmt.Fprintln(os.Stderr, keyApplyInstruction)
	return 0
}

func keyList(c *keyFlags) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	uf, err := auth.LoadUsers(usersPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key list: users file %s: %v\n", usersPath, err)
		return 1
	}
	if len(uf.Keys) == 0 {
		fmt.Println("no keys")
		return 0
	}
	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tEXPIRES\tMODELS")
	for _, k := range uf.Keys {
		status, expires := "enabled", "-"
		if !k.Enabled {
			status = "disabled"
		}
		if k.ExpiresAt != nil {
			expires = k.ExpiresAt.UTC().Format(time.RFC3339)
			if k.Enabled && k.ExpiresAt.Before(now) {
				status = "expired"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, status, expires, strings.Join(k.Models, ", "))
	}
	w.Flush()
	return 0
}

// keyApplyInstruction is the one apply instruction printed after a key
// mutation (D16: one completion line plus one reload action).
const keyApplyInstruction = "Reload Mellomting to apply it: systemctl reload mellomting (or send SIGHUP)."

func keyRevoke(c *keyFlags) int {
	_, usersPath, _, exit := keyState(c)
	if exit != 0 {
		return exit
	}
	err := auth.Update(usersPath, func(uf *auth.UsersFile) error {
		return revoke(uf, c.operand)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "revoked key %s\n", c.operand)
	fmt.Fprintln(os.Stderr, keyApplyInstruction)
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

// splitModels parses the --models list. The wildcard is accepted only on
// its own: a list that mixes it with names is ambiguous about what the
// operator meant to grant.
func splitModels(s string) ([]string, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	wildcard := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == config.ModelWildcard {
			wildcard = true
			continue
		}
		out = append(out, p)
	}
	switch {
	case wildcard && len(out) > 0:
		return nil, fmt.Errorf("--models: %q mixes the wildcard %q with named models; pass either %q alone or the names alone", s, config.ModelWildcard, config.ModelWildcard)
	case wildcard:
		return []string{config.ModelWildcard}, nil
	case len(out) == 0:
		return nil, errors.New("--models: no model named")
	}
	return out, nil
}

// userIDExists reports whether the users file already holds a key with the
// given ID (D18 collision check).
func userIDExists(uf *auth.UsersFile, id string) bool {
	for i := range uf.Keys {
		if uf.Keys[i].ID == id {
			return true
		}
	}
	return false
}

// chooseKeyID returns a freshly generated key whose ID is not already present
// in uf, retrying at most maxAttempts times (D18). A persistent collision or
// a generator (entropy) failure is returned; the users file is not mutated by
// the selection itself. The generator is injectable so the bounded-retry
// behaviour is testable without real randomness.
func chooseKeyID(generate func() (key, id string, err error), uf *auth.UsersFile, maxAttempts int) (string, string, error) {
	for range maxAttempts {
		key, id, err := generate()
		if err != nil {
			return "", "", err
		}
		if !userIDExists(uf, id) {
			return key, id, nil
		}
	}
	return "", "", fmt.Errorf("key id collision after %d attempts", maxAttempts)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: mellomting <command> [flags]

  init       Create configuration from inference servers
  serve      Run the proxy
  key        Manage API keys
  usage      Report token usage
  config     Check or inspect configuration
  sandbox    Check Landlock support
  install    Install the binary or systemd service
  version    Show version
  help       Show this help

Run 'mellomting <command> --help' for details.
`)
}
