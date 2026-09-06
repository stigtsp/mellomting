package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/discovery"
	"mellomting/internal/landlock"
	"mellomting/internal/sandbox"
	"mellomting/internal/seatbelt"
)

// initSandboxCheck is the production capability probe for this
// platform's sandbox. Tests
// replace it so supported, too-old, and unsupported-host behavior is
// deterministic on every CI platform.
var initSandboxCheck = func() sandbox.Report {
	if runtime.GOOS == "darwin" {
		return seatbelt.Check()
	}
	return landlock.Check()
}

const defaultInitListen = "127.0.0.1:8080"

var (
	initServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	initServerNameLike    = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

// serverList is a repeatable flag value for --server.
type serverList []string

func (s *serverList) String() string { return strings.Join(*s, ",") }

func (s *serverList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// initListener is the parsed D17 listener value.
type initListener struct {
	Network        string
	Address        string
	Host           string
	Port           int
	Path           string
	AllowPlaintext bool
}

// initArguments is the fully parsed, preflight-validated form of
// `mellomting init`.
type initArguments struct {
	ConfigPath  string
	UsersPath   string
	PepperPath  string
	Listener    initListener
	Sandbox     initSandbox
	SandboxMode string
	DryRun      bool
	Servers     []discovery.Server
}

// initCmd runs `mellomting init`.
//
// Exit codes: 0 ok, 1 platform/preflight failure, 2 usage error.
func initCmd(args []string) int {
	fs := commandFlags("init", "Create configuration and auth files from inference servers.\nExisting files are never overwritten. Writing them is Linux-only;\n--dry-run prints the configuration on any platform.")
	var servers serverList
	var configPath, listen, sandboxMode string
	var dryRun bool
	fs.Var(&servers, "server", "required server `URL` or NAME=URL; repeat for multiple servers")
	fs.StringVar(&configPath, "config", "", "destination `PATH` (default ./config.yaml)")
	fs.StringVar(&listen, "listen", defaultInitListen, "listener `ADDRESS`: IP:port or absolute socket path")
	fs.StringVar(&sandboxMode, "sandbox", sandbox.ModeBestEffort, "sandbox `MODE` for this platform: required, best-effort, disabled")
	fs.BoolVar(&dryRun, "dry-run", false, "validate and print config without writing files")
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: init: unexpected arguments %q\n", fs.Args())
		return 2
	}
	sandboxSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sandbox" {
			sandboxSet = true
		}
	})

	parsed, err := parseInitArguments(configPath, listen, sandboxMode, sandboxSet, dryRun, []string(servers))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 2
	}
	if err := parsed.Sandbox.preflight(parsed.SandboxMode, sandboxSet, initSandboxCheck); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	if !parsed.DryRun && !initCommitSupported {
		fmt.Fprintf(os.Stderr, "mellomting: init: writing files is Linux-only on this build; rerun with --dry-run to print the configuration\n")
		return 1
	}
	return runInit(parsed)
}

// runInit performs discovery, aggregation, rendering, and commit (B9). It is
// non-interactive: a --dry-run writes the exact config YAML to stdout and
// bounded discovery context to stderr and touches no files; otherwise it
// prints a bounded routing summary, writes the artifacts, and prints one
// completion line plus two next commands.
func runInit(args initArguments) int {
	policy, err := discovery.DerivePolicy(args.Servers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	results, err := discoverInitServers(context.Background(), args.Servers, policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	aggregate, err := discovery.Aggregate(args.Servers, results)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	artifacts, err := renderInitArtifacts(args, aggregate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}

	if args.DryRun {
		if _, err := os.Stdout.Write(artifacts.Config); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: init: cannot write output: %v\n", err)
			return 1
		}
		if err := printInitSummary(os.Stderr, aggregate); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: init: cannot write output: %v\n", err)
			return 1
		}
		return 0
	}

	// The pre-publication summary must be writable before any filesystem
	// mutation (D10).
	if err := printInitSummary(os.Stdout, aggregate); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: cannot write output: %v\n", err)
		return 1
	}
	if err := commitInitArtifacts(args, artifacts, nil); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	if err := printInitCompletion(os.Stdout, args); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: cannot write output: %v\n", err)
		return 1
	}
	return 0
}

func discoverInitServers(ctx context.Context, servers []discovery.Server, policy config.BackendNetwork) (map[string][]string, error) {
	opts := discovery.Options{Network: policy}
	results := make(map[string][]string, len(servers))
	for _, s := range servers {
		ids, err := discovery.Fetch(ctx, s, opts)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", s.Name, err)
		}
		results[s.Name] = ids
	}
	return results, nil
}

// printInitSummary renders the bounded discovered-routing table (D10/B9):
// at most 20 model rows plus an omitted count. It never prints pepper or
// credential contents.
func printInitSummary(w io.Writer, aggregate discovery.Result) error {
	models := aggregate.SortedModels()
	const limit = 20
	if _, err := fmt.Fprintf(w, "discovered %s:\n", plural(len(models), "model")); err != nil {
		return err
	}
	for i, name := range models {
		if i >= limit {
			if _, err := fmt.Fprintf(w, "  (%d more omitted)\n", len(models)-limit); err != nil {
				return err
			}
			break
		}
		if _, err := fmt.Fprintf(w, "  %s: %s\n", name, strings.Join(aggregate.Models[name], ", ")); err != nil {
			return err
		}
	}
	return nil
}

// printInitCompletion prints exactly one completion line and two next
// commands (B9). It never prints pepper or credential contents.
func printInitCompletion(w io.Writer, args initArguments) error {
	_, err := fmt.Fprintf(w, "initialized %s\nnext:\n  mellomting key create local --config %s\n  mellomting serve --config %s\n",
		args.ConfigPath, args.ConfigPath, args.ConfigPath)
	return err
}

func parseInitArguments(configPath, listen, sandboxMode string, sandboxSet, dryRun bool, rawServers []string) (initArguments, error) {
	if len(rawServers) == 0 {
		return initArguments{}, fmt.Errorf("at least one --server is required\nexample: mellomting init --server http://127.0.0.1:8000")
	}
	servers, err := parseInitServers(rawServers)
	if err != nil {
		return initArguments{}, err
	}
	if listen == "" {
		listen = defaultInitListen
	}
	listener, err := parseInitListen(listen)
	if err != nil {
		return initArguments{}, err
	}
	if !sandboxSet {
		sandboxMode = sandbox.ModeBestEffort
	}
	switch sandboxMode {
	case sandbox.ModeRequired, sandbox.ModeBestEffort, sandbox.ModeDisabled:
	default:
		return initArguments{}, fmt.Errorf("--sandbox must be required, best-effort, or disabled")
	}
	cfgPath, usersPath, pepperPath, err := resolveInitAuthPaths(configPath)
	if err != nil {
		return initArguments{}, err
	}
	return initArguments{
		ConfigPath:  cfgPath,
		UsersPath:   usersPath,
		PepperPath:  pepperPath,
		Listener:    listener,
		Sandbox:     initSandboxFor(runtime.GOOS),
		SandboxMode: sandboxMode,
		DryRun:      dryRun,
		Servers:     servers,
	}, nil
}

func parseInitServers(raw []string) ([]discovery.Server, error) {
	if len(raw) > discovery.MaxServers {
		return nil, fmt.Errorf("at most %d --server values are accepted", discovery.MaxServers)
	}
	type parsedServer struct {
		name     string
		explicit bool
		url      string
	}
	parsed := make([]parsedServer, 0, len(raw))
	anyExplicit := false
	for _, v := range raw {
		p := parsedServer{url: v}
		if before, after, ok := strings.Cut(v, "="); ok {
			prefix := before
			if prefix == "" || initServerNameLike.MatchString(prefix) {
				p.name = prefix
				p.url = after
				p.explicit = true
				anyExplicit = true
			}
		}
		parsed = append(parsed, p)
	}
	if anyExplicit {
		for i, p := range parsed {
			if !p.explicit {
				return nil, fmt.Errorf("server %d: if any server has a name, all servers must have names", i+1)
			}
		}
	} else if len(parsed) == 1 {
		parsed[0].name = "local"
	} else {
		for i := range parsed {
			parsed[i].name = fmt.Sprintf("local-%d", i+1)
		}
	}

	names := make(map[string]bool, len(parsed))
	destinations := make(map[string]bool, len(parsed))
	out := make([]discovery.Server, 0, len(parsed))
	for i, p := range parsed {
		if !initServerNamePattern.MatchString(p.name) {
			return nil, fmt.Errorf("server %d: name %q must match ^[a-z][a-z0-9-]{0,62}$", i+1, p.name)
		}
		if names[p.name] {
			return nil, fmt.Errorf("server %d: duplicate name %q", i+1, p.name)
		}
		names[p.name] = true

		u, err := backend.ParseBaseURL(p.url)
		if err != nil {
			return nil, fmt.Errorf("server %d: %v", i+1, err)
		}
		if u.Path != "" {
			return nil, fmt.Errorf("server %d: URL must not contain a path", i+1)
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("server %d: URL host must be a literal IP address", i+1)
		}
		port := u.Port()
		if port == "" {
			return nil, fmt.Errorf("server %d: URL must include an explicit port", i+1)
		}
		portNum, err := strconv.Atoi(port)
		if err != nil || portNum < 1 || portNum > 65535 {
			return nil, fmt.Errorf("server %d: URL port must be decimal in 1..65535", i+1)
		}
		destination := u.Scheme + "|" + canonicalInitIP(ip) + "|" + strconv.Itoa(portNum)
		if destinations[destination] {
			return nil, fmt.Errorf("server %d: duplicate canonical destination %s://%s:%d", i+1, u.Scheme, canonicalInitIP(ip), portNum)
		}
		destinations[destination] = true
		out = append(out, discovery.Server{Name: p.name, BaseURL: p.url})
	}
	return out, nil
}

func canonicalInitIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func parseInitListen(raw string) (initListener, error) {
	if raw == "" {
		return initListener{}, fmt.Errorf("--listen is required")
	}
	if strings.HasPrefix(raw, "/") {
		if !filepath.IsAbs(raw) {
			return initListener{}, fmt.Errorf("--listen Unix path must be absolute")
		}
		cleaned := filepath.Clean(raw)
		if cleaned != raw || cleaned == "/" {
			return initListener{}, fmt.Errorf("--listen Unix path must be an absolute cleaned path")
		}
		return initListener{
			Network: "unix",
			Address: cleaned,
			Path:    cleaned,
		}, nil
	}

	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		if strings.Count(raw, ":") >= 3 && !strings.Contains(raw, "/") && !strings.Contains(raw, ".") {
			return initListener{}, fmt.Errorf("--listen IPv6 host must be bracketed")
		}
		return initListener{}, fmt.Errorf("--listen must be host:port or an absolute Unix path")
	}
	if host != "" && strings.Contains(host, ":") && !strings.HasPrefix(raw, "[") {
		return initListener{}, fmt.Errorf("--listen IPv6 host must be bracketed")
	}
	var ip net.IP
	if host != "" {
		ip = net.ParseIP(host)
		if ip == nil {
			return initListener{}, fmt.Errorf("--listen TCP host must be empty or a literal IP address")
		}
	}
	if strings.ContainsAny(portStr, "+-") {
		return initListener{}, fmt.Errorf("--listen port must be decimal in 1..65535")
	}
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return initListener{}, fmt.Errorf("--listen port must be decimal in 1..65535")
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return initListener{}, fmt.Errorf("--listen port must be decimal in 1..65535")
	}

	allowPlaintext := host == ""
	if ip != nil {
		allowPlaintext = !ip.IsLoopback()
	}

	var address string
	switch {
	case host == "":
		address = ":" + strconv.Itoa(port)
	default:
		address = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	}
	return initListener{
		Network:        "tcp",
		Address:        address,
		Host:           host,
		Port:           port,
		AllowPlaintext: allowPlaintext,
	}, nil
}

func resolveInitAuthPaths(configRaw string) (configPath, usersPath, pepperPath string, err error) {
	if configRaw == "" {
		configRaw = config.DefaultConfigPath
	}
	abs, err := filepath.Abs(configRaw)
	if err != nil {
		return "", "", "", fmt.Errorf("--config path: %v", err)
	}
	if !filepath.IsAbs(abs) {
		return "", "", "", fmt.Errorf("--config path must be absolute after resolution")
	}
	dir := filepath.Dir(abs)
	return abs, filepath.Join(dir, "users.yaml"), filepath.Join(dir, "auth.pepper"), nil
}

// initSandbox is the sandbox of the platform init is writing a
// configuration for: the section that governs it (PLAN §53.1). Both
// platforms default to required, so only the section varies.
type initSandbox struct {
	section string
}

const (
	landlockSection = "landlock"
	seatbeltSection = "seatbelt"
)

func initSandboxFor(goos string) initSandbox {
	if goos == "darwin" {
		return initSandbox{section: seatbeltSection}
	}
	return initSandbox{section: landlockSection}
}

// doc renders the sandbox section of the generated configuration. Only
// Landlock has a version floor to pin.
func (s initSandbox) doc(mode string) map[string]any {
	doc := map[string]any{"mode": mode}
	if s.section == landlockSection {
		doc["minimum_abi"] = landlock.DefaultMinimumABI
	}
	return doc
}

// preflight refuses to write a configuration this host could not then
// serve: a required sandbox the platform cannot enforce would make the
// very next command fail closed. Turning the sandbox off entirely stays
// deliberate — best-effort still applies one wherever it can.
func (s initSandbox) preflight(mode string, explicit bool, check func() sandbox.Report) error {
	if mode == sandbox.ModeDisabled {
		if !explicit {
			return fmt.Errorf("--sandbox %s must be supplied explicitly", mode)
		}
		return nil
	}
	if mode != sandbox.ModeRequired {
		return nil
	}
	report := check()
	if !report.Supported {
		return fmt.Errorf("%s required but unavailable: %s; rerun with --sandbox best-effort to apply one wherever the host allows", s.section, report.Reason)
	}
	if s.section == landlockSection && report.KernelABI < landlock.DefaultMinimumABI {
		return fmt.Errorf("landlock kernel ABI %d is below the required minimum %d; rerun with --sandbox best-effort to apply one wherever the host allows", report.KernelABI, landlock.DefaultMinimumABI)
	}
	return nil
}

// initPepperRand is the pepper entropy source. Tests replace it with a
// deterministic source while the production path remains crypto/rand.
var initPepperRand = func(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate pepper: %w", err)
	}
	return b, nil
}

// initArtifacts is the fully rendered and validated in-memory output of
// init. No byte in this value has been written to disk.
type initArtifacts struct {
	Config     []byte
	Users      []byte
	Pepper     []byte
	Normalized *config.Config
}

// renderInitArtifacts builds the exact config, users, and pepper bytes from
// parsed arguments and discovered models (B7). It parses and validates the
// rendered config and users representation before returning, and it never
// touches the filesystem.
func renderInitArtifacts(args initArguments, discovered discovery.Result) (initArtifacts, error) {
	if len(discovered.Models) == 0 {
		return initArtifacts{}, errors.New("discovery returned no models")
	}
	policy, err := discovery.DerivePolicy(args.Servers)
	if err != nil {
		return initArtifacts{}, err
	}

	pepper, err := initPepperRand(auth.PepperBytes)
	if err != nil {
		return initArtifacts{}, err
	}
	if err := auth.ValidatePepper(pepper); err != nil {
		return initArtifacts{}, err
	}
	pepperBytes := []byte(auth.EncodePepper(pepper))

	usersBytes := auth.EmptyUsersBytes()
	if _, err := auth.ParseUsers(usersBytes); err != nil {
		return initArtifacts{}, fmt.Errorf("render empty users file: %w", err)
	}

	configBytes, err := renderInitConfig(args, discovered, policy)
	if err != nil {
		return initArtifacts{}, err
	}
	cfg, err := config.Parse(configBytes)
	if err != nil {
		return initArtifacts{}, fmt.Errorf("rendered configuration failed validation: %w", err)
	}

	return initArtifacts{
		Config:     configBytes,
		Users:      usersBytes,
		Pepper:     pepperBytes,
		Normalized: cfg,
	}, nil
}

func renderInitConfig(args initArguments, discovered discovery.Result, policy config.BackendNetwork) ([]byte, error) {
	listen := map[string]any{
		"network": args.Listener.Network,
		"address": args.Listener.Address,
	}
	if args.Listener.Network == "unix" {
		listen["mode"] = config.DefaultUnixSocketMode
	}
	server := map[string]any{
		"listen": listen,
	}
	if args.Listener.Network == "tcp" && args.Listener.AllowPlaintext {
		server["allow_plaintext_non_loopback"] = true
	}

	var networkDoc map[string]any
	switch policy.Mode {
	case "loopback-only":
		networkDoc = map[string]any{"mode": policy.Mode}
	case "allowed-cidrs":
		networkDoc = map[string]any{
			"mode":  policy.Mode,
			"cidrs": policy.CIDRs,
		}
	default:
		return nil, fmt.Errorf("unsupported derived backend network policy %q", policy.Mode)
	}

	servers := make(map[string]any, len(args.Servers))
	for _, s := range args.Servers {
		servers[s.Name] = map[string]any{"url": s.BaseURL}
	}

	models := make(map[string]any, len(discovered.Models))
	for _, name := range discovered.SortedModels() {
		replicas := append([]string(nil), discovered.Models[name]...)
		models[name] = map[string]any{
			"type":    "generation",
			"servers": replicas,
		}
	}

	doc := map[string]any{
		"version": 1,
		"server":  server,
		"auth": map[string]any{
			"pepper_file": args.PepperPath,
			"users_file":  args.UsersPath,
		},
		"security": map[string]any{
			"backend_network":    networkDoc,
			args.Sandbox.section: args.Sandbox.doc(args.SandboxMode),
		},
		"servers": servers,
		"models":  models,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("render configuration: %w", err)
	}
	return out, nil
}

// commitFileIdentity is the platform-independent identity of one filesystem
// object. It is used to revalidate opened parents and to roll back only the
// inodes this invocation created.
type commitFileIdentity struct {
	Dev  uint64
	Ino  uint64
	Mode uint64
	Uid  uint64
}

// commitOps is the filesystem seam for the D4 init transaction. The
// production implementation is Linux-specific and directory-FD-relative.
type commitOps interface {
	openParent(path string) (fd int, id commitFileIdentity, err error)
	fstatParent(fd int) (commitFileIdentity, error)
	lstatInDir(fd int, name string) (commitFileIdentity, error)
	createInDir(fd int, name string, mode uint32) (fileFD int, id commitFileIdentity, err error)
	writeAll(fd int, name string, data []byte) error
	fsyncFile(fd int, name string) error
	fstatFile(fd int, name string) (commitFileIdentity, error)
	closeFile(fd int, name string) error
	linkInDir(fd int, oldname, newname string) error
	unlinkInDir(fd int, name string) error
	fsyncDir(fd int) error
}

type commitFile struct {
	temp      string
	final     string
	path      string
	data      []byte
	fd        int
	id        commitFileIdentity
	published bool
}

// commitInitArtifacts publishes rendered init artifacts with create-only,
// directory-FD-relative semantics (D4/D5). It never overwrites an existing
// final path and never follows a final-component symlink. A --dry-run
// invocation returns from runInit before reaching this function.
func commitInitArtifacts(args initArguments, artifacts initArtifacts, ops commitOps) error {
	if ops == nil {
		ops = defaultCommitOps()
	}
	if filepath.Dir(args.ConfigPath) != filepath.Dir(args.UsersPath) || filepath.Dir(args.ConfigPath) != filepath.Dir(args.PepperPath) {
		return fmt.Errorf("config, users, and pepper must share one parent directory")
	}
	dir := filepath.Dir(args.ConfigPath)

	dirFD, dirID, err := ops.openParent(dir)
	if err != nil {
		return fmt.Errorf("open destination directory: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = ops.closeFile(dirFD, dir)
		}
	}()

	if uint64(os.Geteuid()) != dirID.Uid {
		return fmt.Errorf("destination directory %s must be owned by the effective user", dir)
	}
	if dirID.Mode&0o022 != 0 {
		return fmt.Errorf("destination directory %s must not be group- or world-writable", dir)
	}
	if id, err := ops.fstatParent(dirFD); err != nil {
		return fmt.Errorf("revalidate destination directory: %w", err)
	} else if id.Dev != dirID.Dev || id.Ino != dirID.Ino {
		return fmt.Errorf("destination directory %s changed after it was opened", dir)
	}

	files := []commitFile{
		{
			temp:  fmt.Sprintf(".mellomting-init-%d-pepper.tmp", os.Getpid()),
			final: filepath.Base(args.PepperPath),
			path:  args.PepperPath,
			data:  artifacts.Pepper,
		},
		{
			temp:  fmt.Sprintf(".mellomting-init-%d-users.tmp", os.Getpid()),
			final: filepath.Base(args.UsersPath),
			path:  args.UsersPath,
			data:  artifacts.Users,
		},
		{
			temp:  fmt.Sprintf(".mellomting-init-%d-config.tmp", os.Getpid()),
			final: filepath.Base(args.ConfigPath),
			path:  args.ConfigPath,
			data:  artifacts.Config,
		},
	}

	var existing []string
	for _, f := range files {
		if _, err := ops.lstatInDir(dirFD, f.final); err == nil {
			existing = append(existing, f.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat destination %s: %w", f.path, err)
		}
	}
	if len(existing) > 0 {
		return fmt.Errorf("refusing to overwrite existing %s; inspect and remove them manually", strings.Join(existing, ", "))
	}

	for i := range files {
		fd, id, err := ops.createInDir(dirFD, files[i].temp, 0o600)
		if err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("create temporary %s: %w", files[i].temp, err)
		}
		files[i].fd = fd
		files[i].id = id
	}

	for i := range files {
		if err := ops.writeAll(files[i].fd, files[i].temp, files[i].data); err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("write %s: %w", files[i].temp, err)
		}
		if err := ops.fsyncFile(files[i].fd, files[i].temp); err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("fsync %s: %w", files[i].temp, err)
		}
		if id, err := ops.fstatFile(files[i].fd, files[i].temp); err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("revalidate %s: %w", files[i].temp, err)
		} else if id.Dev != files[i].id.Dev || id.Ino != files[i].id.Ino {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("%s changed after it was created", files[i].temp)
		}
		if err := ops.closeFile(files[i].fd, files[i].temp); err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return fmt.Errorf("close %s: %w", files[i].temp, err)
		}
		files[i].fd = -1
	}

	// D4 order: publish and unlink the two auth entries, durably sync the
	// directory, then publish the config destination last.
	for i := range 2 {
		if err := publishCommitFile(ops, dirFD, &files[i]); err != nil {
			rollbackCommitFiles(ops, dirFD, files)
			return err
		}
	}
	if err := ops.fsyncDir(dirFD); err != nil {
		rollbackCommitFiles(ops, dirFD, files)
		return fmt.Errorf("fsync destination directory: %w", err)
	}
	if err := publishCommitFile(ops, dirFD, &files[2]); err != nil {
		rollbackCommitFiles(ops, dirFD, files)
		return err
	}
	if err := ops.fsyncDir(dirFD); err != nil {
		rollbackCommitFiles(ops, dirFD, files)
		return fmt.Errorf("fsync destination directory: %w", err)
	}

	closed = true
	return nil
}

func publishCommitFile(ops commitOps, dirFD int, f *commitFile) error {
	id, err := ops.lstatInDir(dirFD, f.temp)
	if err != nil {
		return fmt.Errorf("revalidate %s: %w", f.temp, err)
	}
	if id.Dev != f.id.Dev || id.Ino != f.id.Ino {
		return fmt.Errorf("%s changed after it was created", f.temp)
	}
	if err := ops.linkInDir(dirFD, f.temp, f.final); err != nil {
		return fmt.Errorf("publish %s: %w", f.path, err)
	}
	f.published = true
	if err := ops.unlinkInDir(dirFD, f.temp); err != nil {
		return fmt.Errorf("unlink %s: %w", f.temp, err)
	}
	return nil
}

func rollbackCommitFiles(ops commitOps, dirFD int, files []commitFile) {
	for _, file := range slices.Backward(files) {
		if !file.published {
			continue
		}
		if id, err := ops.lstatInDir(dirFD, file.final); err == nil && id.Dev == file.id.Dev && id.Ino == file.id.Ino {
			_ = ops.unlinkInDir(dirFD, file.final)
		}
	}
	for _, file := range slices.Backward(files) {
		if file.id.Ino == 0 {
			continue
		}
		if id, err := ops.lstatInDir(dirFD, file.temp); err == nil && id.Dev == file.id.Dev && id.Ino == file.id.Ino {
			_ = ops.unlinkInDir(dirFD, file.temp)
		}
	}
}
