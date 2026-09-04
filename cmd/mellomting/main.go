package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/landlock"
	"mellomting/internal/version"
)

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
	case "--install", "-install":
		os.Exit(installCmd(os.Args[2:]))
	case "init":
		os.Exit(initCmd(os.Args[2:]))
	case "config":
		os.Exit(configCmd(os.Args[2:]))
	case "sandbox":
		os.Exit(sandboxCmd(os.Args[2:]))
	case "key":
		os.Exit(keyCmd(os.Args[2:]))
	case "usage":
		os.Exit(usageCmd(os.Args[2:]))
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "mellomting: %s\n", describeUnknownCommand(os.Args[1]))
		usage(os.Stderr)
		os.Exit(2)
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
	sub, rest := subcommand(args, "check")
	switch sub {
	case "check", "show-effective":
	default:
		fmt.Fprintf(os.Stderr, "mellomting: unknown config subcommand %q (want check or show-effective)\n", sub)
		return 2
	}

	fs := flag.NewFlagSet("mellomting config "+sub, flag.ContinueOnError)
	var configPath string
	fs.StringVar(&configPath, "config", "", "configuration file path")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	resolved, err := ResolveConfigPath(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: config %s: %v\n", sub, err)
		return 1
	}

	if sub == "check" {
		if _, err := config.Load(resolved); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: config %s failed: %v\n", sub, err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "mellomting: configuration at %s is valid\n", resolved)
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
	sub, rest := subcommand(args, "check")
	if sub != "check" {
		fmt.Fprintf(os.Stderr, "mellomting: unknown sandbox subcommand %q (want check)\n", sub)
		return 2
	}

	fs := flag.NewFlagSet("mellomting sandbox check", flag.ContinueOnError)
	var configPath string
	fs.StringVar(&configPath, "config", "", "configuration file for landlock mode/minimum_abi (optional)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	resolved, err := ResolveConfigPath(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: sandbox check: %v\n", err)
		return 1
	}

	report := landlock.Check()
	mode := landlock.ModeRequired
	minABI := landlock.DefaultMinimumABI
	enforce := false
	local := resolved == "./config.yaml"
	if cfg, loadErr := config.Load(resolved); loadErr == nil {
		mode = cfg.Security.Landlock.Mode
		minABI = cfg.Security.Landlock.MinimumABI
		enforce = true
	} else if local || configPath != "" {
		// A discovered local ./config.yaml or an explicit --config must be
		// loadable; report the failure rather than silently running
		// report-only. An absent or unreadable system default (/etc) is
		// allowed to fall back to report-only so the check works without a
		// configuration.
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
	configPath         string
	name               string
	models             string
	expires            string
	id                 string
	concurrentRequests int
	requestsPerSecond  float64
	burst              int
	limitsSet          bool
}

func keyParseFlags(sub string, rest []string) (*keyFlags, int) {
	fs := flag.NewFlagSet("mellomting key "+sub, flag.ContinueOnError)
	c := &keyFlags{}
	fs.StringVar(&c.configPath, "config", "", "configuration file path")
	if sub == "create" {
		fs.StringVar(&c.name, "name", "", "human-readable key name (required)")
		fs.StringVar(&c.models, "models", "", "comma-separated model names, or * (optional; inferred when exactly one model is configured)")
		fs.StringVar(&c.expires, "expires", "", "expiry as RFC3339 (optional)")
		fs.IntVar(&c.concurrentRequests, "concurrent-requests", 0, "max simultaneous in-flight requests (0: apply default)")
		fs.Float64Var(&c.requestsPerSecond, "requests-per-second", 0, "request rate limit (0: no rate limit)")
		fs.IntVar(&c.burst, "burst", 0, "rate-limit burst (defaults to one second of rate)")
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
	if sub == "create" {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "concurrent-requests", "requests-per-second", "burst":
				c.limitsSet = true
			}
		})
	}

	switch sub {
	case "create":
		if c.name == "" {
			fmt.Fprintln(os.Stderr, "mellomting: key create: --name is required")
			return nil, 2
		}
		if c.expires != "" {
			if _, err := time.Parse(time.RFC3339, c.expires); err != nil {
				fmt.Fprintf(os.Stderr, "mellomting: key create: invalid --expires %q (want RFC3339)\n", c.expires)
				return nil, 2
			}
		}
		// Per-key limits fail closed on negative values (T-X8); the daemon
		// rejects them at the key-store boundary, the CLI must not emit
		// them in the first place.
		if c.concurrentRequests < 0 {
			fmt.Fprintf(os.Stderr, "mellomting: key create: --concurrent-requests must be >= 0, got %d\n", c.concurrentRequests)
			return nil, 2
		}
		if c.requestsPerSecond < 0 {
			fmt.Fprintf(os.Stderr, "mellomting: key create: --requests-per-second must be >= 0, got %v\n", c.requestsPerSecond)
			return nil, 2
		}
		if c.burst < 0 {
			fmt.Fprintf(os.Stderr, "mellomting: key create: --burst must be >= 0, got %d\n", c.burst)
			return nil, 2
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
	resolved, err := ResolveConfigPath(c.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key: %v\n", err)
		return nil, "", "", 1
	}
	cfg, err = config.Load(resolved)
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
	key, id, err := auth.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
		return 1
	}

	var models []string
	if c.models == "" {
		// D14: infer the model only when the config has exactly one
		// public model; otherwise fail and point the operator at
		// --models. Never infer "*".
		inferred, err := inferSoleModel(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
			return 2
		}
		models = inferred
	} else {
		models = splitModels(c.models)
		// A model not present in the loaded config is legal (an ACL may
		// name a model about to be added), but the operator should hear
		// about it (T-L4).
		for _, m := range models {
			if m == "*" {
				continue
			}
			if _, ok := cfg.Models[m]; !ok {
				fmt.Fprintf(os.Stderr, "mellomting: key create: warning: model %q is not present in the configuration; the ACL will allow it once the model exists\n", m)
			}
		}
	}
	var exp *time.Time
	if c.expires != "" {
		t, err := time.Parse(time.RFC3339, c.expires)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: key create: %v\n", err)
			return 1
		}
		exp = &t
	}
	limits := auth.KeyLimits{
		ConcurrentRequests: c.concurrentRequests,
		RequestsPerSecond:  c.requestsPerSecond,
		Burst:              c.burst,
	}

	err = auth.Update(usersPath, func(uf *auth.UsersFile) error {
		uf.Keys = append(uf.Keys, auth.Key{
			ID:         id,
			Name:       c.name,
			SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)),
			Enabled:    true,
			ExpiresAt:  exp,
			Models:     models,
			Limits:     limits,
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
	if c.limitsSet {
		fmt.Printf("  concurrent_requests: %d\n", c.concurrentRequests)
		fmt.Printf("  requests_per_second: %v\n", c.requestsPerSecond)
		fmt.Printf("  burst:               %d\n", c.burst)
	}
	fmt.Printf("  users file:        %s\n", usersPath)
	fmt.Fprintln(os.Stdout, "Store the secret now; it is not retrievable later.")
	warnLandlockReloadRequired(cfg, "key create")
	return 0
}

func keySetEnabled(c *keyFlags, enable bool) int {
	cfg, usersPath, _, exit := keyState(c)
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
	warnLandlockReloadRequired(cfg, "key "+verb)
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

// warnLandlockReloadRequired warns, when the configured Landlock mode is
// required, that a running server applies users-file changes only after
// a restart. Under that mode the users file is pinned to its startup
// inode (PLAN §58) and the offline key commands atomically rename it, so
// a SIGHUP reload of the change is denied by the sandbox and fails
// closed (PLAN §30) — the daemon logs the sandbox ERROR naming the
// restart requirement if one is attempted (FIX-02/N2). A revoked (or
// disabled) key stays effective on a live server until a restart, so a
// silent success here would be a false sense of security (FIX-02 eval
// residual). The warning therefore never recommends SIGHUP.
func warnLandlockReloadRequired(cfg *config.Config, action string) {
	if cfg.Security.Landlock.Mode != landlock.ModeRequired {
		return
	}
	fmt.Fprintf(os.Stderr,
		"mellomting: warning: %s under landlock.mode=required: running servers apply users-file changes only after a restart; the change is not effective on a live server until then\n",
		action)
}

func keyRevoke(c *keyFlags) int {
	cfg, usersPath, _, exit := keyState(c)
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
	warnLandlockReloadRequired(cfg, "key revoke")
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

// inferSoleModel resolves the key's model list when --models is absent
// (D14): exactly one configured public model is inferred; zero or two or
// more models fail, listing at most 20 model names plus the omitted count.
// It never infers "*".
func inferSoleModel(cfg *config.Config) ([]string, error) {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return nil, fmt.Errorf("no models are configured; pass --models explicitly")
	case 1:
		return names, nil
	default:
		const limit = 20
		shown := names
		if len(names) > limit {
			shown = names[:limit]
		}
		msg := "multiple models are configured; pass --models explicitly. configured: " + strings.Join(shown, ", ")
		if len(names) > limit {
			msg += fmt.Sprintf(" (and %d more; run `mellomting config show-effective` for the complete set)", len(names)-limit)
		}
		return nil, fmt.Errorf("%s", msg)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: mellomting <command> [flags]

commands:
  version                      print version information
  help                         print this help
  init --server URL|NAME=URL   prepare local config and auth files from
                               explicitly supplied inference servers
  config check                 validate configuration (exit 0 when valid)
  config show-effective        print the effective configuration with defaults
  sandbox check                report Landlock capability and policy result
  key <subcommand>             offline API key management

key subcommands:
  key create   --name NAME [--models M[,M...]] [--expires RFC3339]
               [--concurrent-requests N] [--requests-per-second R] [--burst B]
               create a key and print it once; --models is optional and is
               inferred when exactly one model is configured (never "*");
               without limits flags a conservative concurrency default applies
  key list                            list keys (id, name, models, status)
  key enable  --id ID                 re-enable a key
  key disable --id ID                 disable a key
  key revoke  --id ID                 permanently remove a key
  all key subcommands accept -config PATH (default: mellomting config path)

  serve -config PATH           run the proxy daemon (default: config path)
  usage report                 report per-key token/request usage
  --install [--prefix DIR]     copy this binary to DIR/bin (default DIR:
                                /usr/local): atomic replace, mode 0755,
                                symlink destinations refused
  --install --systemd          also provision as a systemd service (Linux
                                root): service user, config/log/state/run
                                dirs, scaffold config.yaml, generated
                                pepper, and empty users.yaml (each if
                                absent), hardened unit + logrotate, and
                                a daemon-reload
 `)
}
