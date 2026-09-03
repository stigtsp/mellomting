package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/discovery"
	"mellomting/internal/landlock"
)

// initLandlockCheck is the production Landlock capability probe. Tests
// replace it so supported, too-old, and unsupported-host behavior is
// deterministic on every CI platform.
var initLandlockCheck = landlock.Check

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
	ConfigPath   string
	UsersPath    string
	PepperPath   string
	Listener     initListener
	LandlockMode string
	DryRun       bool
	Servers      []discovery.Server
}

// initCmd runs `mellomting init`.
//
// Exit codes: 0 ok, 1 platform/preflight failure, 2 usage error.
func initCmd(args []string) int {
	fs := flag.NewFlagSet("mellomting init", flag.ContinueOnError)
	var servers serverList
	var configPath, listen, landlockMode string
	var dryRun bool
	fs.Var(&servers, "server", "backend server URL or NAME=URL (repeatable)")
	fs.StringVar(&configPath, "config", "", "config destination path")
	fs.StringVar(&listen, "listen", defaultInitListen, "listener address")
	fs.StringVar(&landlockMode, "landlock", landlock.ModeRequired, "landlock mode: required, best-effort, or disabled")
	fs.BoolVar(&dryRun, "dry-run", false, "validate and print config without writing files")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: init: unexpected arguments %q\n", fs.Args())
		return 2
	}
	landlockSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "landlock" {
			landlockSet = true
		}
	})

	parsed, err := parseInitArguments(configPath, listen, landlockMode, landlockSet, dryRun, []string(servers))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 2
	}
	if err := initPreflightLandlock(parsed.LandlockMode, landlockSet, initLandlockCheck); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	return 0
}

func parseInitArguments(configPath, listen, landlockMode string, landlockSet, dryRun bool, rawServers []string) (initArguments, error) {
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
	if !landlockSet {
		// D2: the default is required; the operator has not opted out.
		landlockMode = landlock.ModeRequired
	}
	switch landlockMode {
	case landlock.ModeRequired, landlock.ModeBestEffort, landlock.ModeDisabled:
	default:
		return initArguments{}, fmt.Errorf("--landlock must be required, best-effort, or disabled")
	}
	cfgPath, usersPath, pepperPath, err := resolveInitAuthPaths(configPath)
	if err != nil {
		return initArguments{}, err
	}
	return initArguments{
		ConfigPath:   cfgPath,
		UsersPath:    usersPath,
		PepperPath:   pepperPath,
		Listener:     listener,
		LandlockMode: landlockMode,
		DryRun:       dryRun,
		Servers:      servers,
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
		if eq := strings.IndexByte(v, '='); eq >= 0 {
			prefix := v[:eq]
			if prefix == "" || initServerNameLike.MatchString(prefix) {
				p.name = prefix
				p.url = v[eq+1:]
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

func initPreflightLandlock(mode string, explicit bool, check func() landlock.Report) error {
	report := check()
	if mode == landlock.ModeRequired {
		if !report.Supported {
			return fmt.Errorf("Landlock required but unavailable: %s; rerun with --landlock best-effort to continue without the sandbox", report.Reason)
		}
		if report.KernelABI < landlock.DefaultMinimumABI {
			return fmt.Errorf("Landlock kernel ABI %d below required minimum %d; rerun with --landlock best-effort to continue without the sandbox", report.KernelABI, landlock.DefaultMinimumABI)
		}
	}
	if mode == landlock.ModeBestEffort || mode == landlock.ModeDisabled {
		if !explicit {
			return fmt.Errorf("--landlock %s must be supplied explicitly", mode)
		}
	}
	return nil
}
