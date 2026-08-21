// Command serve: run the Mellomting proxy daemon (PLAN §92).
//
// Startup is fail-closed: configuration, the key store, the pepper,
// backend URL/validation, and (when required) the sandbox gate must all
// succeed before the listener accepts traffic. On SIGINT/SIGTERM the
// daemon performs the graceful shutdown sequence (PLAN §74).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"mellomting/internal/accounting"
	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/landlock"
	"mellomting/internal/logging"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
	"mellomting/internal/tlsconfig"
	"mellomting/internal/version"
)

// daemon holds the fully-wired components of one Mellomting instance.
type daemon struct {
	cfg       *config.Config
	log       *slog.Logger
	api       *httpapi.Server
	proxy     *proxy.Proxy
	acc       *accounting.Writer // usage JSONL writer; nil when disabled
	tlsConfig *tls.Config        // static listener TLS (PLAN §67); nil when absent
	listen    net.Listener
}

// serveCmd runs the proxy daemon.
//
// Exit codes: 0 clean shutdown, 1 startup or shutdown failure,
// 2 usage error.
func serveCmd(args []string) int {
	fs := flag.NewFlagSet("mellomting serve", flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultConfigPath, "configuration file path")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: serve: invalid configuration: %v\n", err)
		return 1
	}
	log, err := newDaemonLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: serve: invalid logging configuration: %v\n", err)
		return 1
	}

	if err := rejectShelvedFeatures(cfg); err != nil {
		log.Error("startup rejected: shelved feature configured", "error_class", "configuration")
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	d, err := buildDaemon(cfg, log)
	if err != nil {
		log.Error("startup failed", "error_class", "startup")
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	ln, err := buildListener(cfg.Server.Listen)
	if err != nil {
		log.Error("listener failed", "error_class", "listener", "network", cfg.Server.Listen.Network)
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}
	// Static TLS (PLAN §67): wrap the listener before the sandbox is
	// applied. The certificate and key were already loaded with the rest
	// of the startup secrets (PLAN §57 step 9).
	if d.tlsConfig != nil {
		ln = tls.NewListener(ln, d.tlsConfig)
	}
	d.listen = ln
	defer os.Remove(d.listenAddr()) // best-effort socket cleanup

	if cfg.Server.TLS.Mode == "" && cfg.Server.Listen.Network == "tcp" &&
		isNonLoopbackListenAddr(cfg.Server.Listen.Address) &&
		cfg.Server.AllowPlaintextNonLoopback {
		log.Warn("plaintext non-loopback TCP listener is active (PLAN §8.2)")
	}

	// Landlock confinement (PLAN §55-63, §95). This is the last step
	// before the listener accepts (PLAN §57 step 19): the policy is
	// applied to every runtime thread and verified before any client
	// request may be processed. No config/secret file descriptors are
	// open here (auth.LoadUsers/LoadPepper and securefile.Read close
	// their own FDs), so the open-file caveat (PLAN §59) is satisfied.
	if err := applySandbox(cfg, log); err != nil {
		log.Error("sandbox enforcement failed", "error_class", "landlock")
		_ = ln.Close()
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	srv := newHTTPServer(cfg, d.api)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	d.api.SetReady(true)
	log.Info("mellomting ready",
		"version", version.String(),
		"network", cfg.Server.Listen.Network,
		"address", cfg.Server.Listen.Address,
		"tls", d.tlsConfig != nil,
		"backends", len(cfg.Backends),
		"models", len(cfg.Models),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error_class", "server")
			return 1
		}
		return 0
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace_period", cfg.Shutdown.GracePeriod.Duration().String())
	// PLAN §74: 1-2. stop accepting, reject new admissions.
	d.proxy.BeginDraining()
	// 3. let active streams finish up to the deadline.
	timeout := cfg.Shutdown.GracePeriod.Duration()
	gctx, cancel := context.WithTimeout(context.Background(), timeout)
	_ = srv.Shutdown(gctx)
	cancel()
	// 4. cancel whatever remains.
	srv.Close()
	// 7. close listener.
	if err := ln.Close(); err != nil {
		log.Warn("listener close", "error", err)
	}
	// 8. flush and close the accounting writer (PLAN §42).
	if d.acc != nil {
		if err := d.acc.Close(); err != nil {
			log.Warn("accounting close", "error", err)
		}
	}
	log.Info("shutdown complete")
	return 0
}

// newDaemonLogger builds the structured logger per PLAN §43.
func newDaemonLogger(cfg *config.Config) (*slog.Logger, error) {
	return logging.New(os.Stdout, cfg.Logging.Level, cfg.Logging.Format)
}

// rejectShelvedFeatures fails closed on features that are shelved for the
// first release (PLAN §68, §96). The configuration schema stays
// forward-compatible, but serve refuses to start with them enabled so a
// shelved feature is never silently disabled.
func rejectShelvedFeatures(cfg *config.Config) error {
	var shelved []string
	if len(cfg.Qualifiers) > 0 {
		shelved = append(shelved, "qualifiers (PLAN §96)")
	}
	if cfg.Server.TLS.Mode == "acme" {
		shelved = append(shelved, `tls.mode "acme" (PLAN §68)`)
	}
	if len(shelved) > 0 {
		return fmt.Errorf("%s are shelved for the first release: remove them from the configuration and retry",
			strings.Join(shelved, " and "))
	}
	return nil
}

// applySandbox builds the post-startup policy from the validated
// configuration and enforces it on every runtime thread (PLAN §55-63,
// §95). It runs after all listener/secret file descriptors are settled
// and before the listener accepts (PLAN §57 step 19); no client request
// may be processed before it returns nil in required mode.
//
// Mode semantics (PLAN §55):
//   - "disabled":     no sandbox; an informational log.
//   - "best-effort":  enforce the full policy, or continue with a
//     warning when the policy cannot be applied. The daemon never runs
//     under a partially degraded policy.
//   - "required":     enforce the full policy or fail startup. It never
//     falls back to the library's BestEffort() downgrade, which could
//     degrade to no protection (PLAN §55 step 5).
//
// The policy is the minimal post-startup right set (PLAN §58): read the
// users file (SIGHUP reload, PLAN §30), write the accounting log, and
// connect to the configured backend TCP ports. Secrets are preloaded
// and their FDs closed before this runs (PLAN §59).
func applySandbox(cfg *config.Config, log *slog.Logger) error {
	l := cfg.Security.Landlock
	if l.Mode == landlock.ModeDisabled {
		log.Info("landlock disabled by configuration")
		return nil
	}
	required := l.Mode == landlock.ModeRequired

	// Build the policy (PLAN §58, §60, §62).
	ports, err := landlock.BackendPorts(backendBaseURLs(cfg)...)
	if err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	pol := landlock.Policy{
		ReadFiles:  []string{cfg.Auth.UsersFile},
		ConnectTCP: ports,
	}
	if cfg.Accounting.Enabled {
		pol.WriteFiles = append(pol.WriteFiles, cfg.Accounting.Path)
	}

	failClosed := func(reason, detail string) error {
		if required {
			msg := reason
			if detail != "" {
				msg += ": " + detail
			}
			return fmt.Errorf("security.landlock.mode is %q but the sandbox cannot be enforced: %s", l.Mode, msg)
		}
		log.Warn("landlock: not applied (continuing without a sandbox)", "reason", reason, "detail", detail)
		return nil
	}

	report := landlock.Check()
	if !report.Supported {
		return failClosed("sandbox cannot be enforced", report.Reason)
	}
	if report.KernelABI < l.MinimumABI {
		return failClosed(
			"kernel Landlock ABI is below the configured minimum",
			fmt.Sprintf("kernel ABI %d < minimum_abi %d", report.KernelABI, l.MinimumABI))
	}
	// Enforce at the highest ABI supported by both the kernel and the
	// pinned library (PLAN §55 step 3).
	abi := report.KernelABI
	if abi > landlock.MaxABI {
		abi = landlock.MaxABI
	}
	if err := landlock.Apply(abi, pol); err != nil {
		return failClosed("sandbox application failed", err.Error())
	}
	log.Info("landlock enforced",
		"mode", l.Mode,
		"kernel_abi", report.KernelABI,
		"applied_abi", abi,
		"rules", pol.Summarize(),
	)
	return nil
}

// backendBaseURLs returns the configured backend base URLs (PLAN §60).
func backendBaseURLs(cfg *config.Config) []string {
	urls := make([]string, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		urls = append(urls, b.BaseURL)
	}
	return urls
}

// buildListener creates the ingress listener (PLAN §8).
func buildListener(l config.Listen) (net.Listener, error) {
	switch l.Network {
	case "unix":
		return safeUnixListen(l)
	default:
		return mptcpOffListen(l.Address)
	}
}

// mptcpOffListen listens on TCP with MPTCP explicitly disabled
// (PLAN §61, §83).
func mptcpOffListen(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	return lc.Listen(context.Background(), "tcp", addr)
}

// safeUnixListen creates a pathname Unix socket per PLAN §8.3: it
// refuses to replace a symlink or any non-socket file, removes a stale
// socket, and applies the configured mode after bind. An empty mode takes
// the PLAN §8.1 default (0660).
func safeUnixListen(l config.Listen) (net.Listener, error) {
	modeStr := l.Mode
	if l.Mode == "" {
		modeStr = config.DefaultUnixSocketMode
	}
	mode, err := strconv.ParseUint(modeStr, 8, 16)
	if err != nil || mode == 0 || mode > 0o777 {
		return nil, fmt.Errorf("unix socket mode %q must be octal (e.g. 0660)", modeStr)
	}
	if st, err := os.Lstat(l.Address); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing to unlink %q: it is a symlink (PLAN §8.3)", l.Address)
		}
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to unlink %q: it is not a unix socket (PLAN §8.3)", l.Address)
		}
		if err := os.Remove(l.Address); err != nil {
			return nil, fmt.Errorf("remove stale socket %q: %w", l.Address, err)
		}
	}
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(context.Background(), "unix", l.Address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(l.Address, os.FileMode(mode)); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %q: %w", l.Address, err)
	}
	return ln, nil
}

// newHTTPServer builds the net/http server with the PLAN §9.1 timeouts.
// There is deliberately no WriteTimeout (it would break long LLM streams,
// PLAN §9.1); per-stream idle bounds are enforced in the proxy.
// ReadTimeout bounds the full request read (headers + body) by the sum
// of the header and body budgets, the closest available approximation.
func newHTTPServer(cfg *config.Config, api *httpapi.Server) *http.Server {
	return &http.Server{
		Handler:           api.Handler(),
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		ReadTimeout:       cfg.Server.ReadHeaderTimeout.Duration() + cfg.Server.ReadBodyTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
	}
}

// buildNetworkPolicy parses the backend egress policy (PLAN §16).
func buildNetworkPolicy(bn config.BackendNetwork) (backend.Policy, error) {
	var p backend.Policy
	p.Mode = bn.Mode
	for _, cidr := range bn.CIDRs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return backend.Policy{}, fmt.Errorf("security.backend_network.cidrs: %q is not a CIDR", cidr)
		}
		p.CIDRs = append(p.CIDRs, ipnet)
	}
	return p, nil
}

// buildDaemon wires the key store, router, backend clients, proxy, and
// HTTP surface from validated configuration. It performs no I/O beyond
// reading the key store, pepper, backend credentials, and the static TLS
// certificate and key (PLAN §57 step 9); all of those FDs are closed
// before the sandbox is applied.
func buildDaemon(cfg *config.Config, log *slog.Logger) (*daemon, error) {
	users, err := auth.LoadUsers(cfg.Auth.UsersFile)
	if err != nil {
		return nil, fmt.Errorf("load users file: %w", err)
	}
	pepper, err := auth.LoadPepper(cfg.Auth.PepperFile)
	if err != nil {
		return nil, fmt.Errorf("load pepper: %w", err)
	}
	store, err := auth.NewStore(users, pepper)
	if err != nil {
		return nil, fmt.Errorf("key store: %w", err)
	}

	policy, err := buildNetworkPolicy(cfg.Security.BackendNetwork)
	if err != nil {
		return nil, err
	}

	// Static TLS (PLAN §57 step 9, §67): the certificate and key are
	// loaded once, before the sandbox, and are reloaded only by a process
	// restart in v1.
	var tlsConfig *tls.Config
	if cfg.Server.TLS.Mode == "files" {
		tlsCfg, err := tlsconfig.Files(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig = tlsCfg
		log.Info("static tls configured",
			"cert_file", cfg.Server.TLS.CertFile,
			"key_file", cfg.Server.TLS.KeyFile,
		)
	}

	clients := make(map[string]*backend.Client, len(cfg.Backends))
	for name, b := range cfg.Backends {
		client, err := backend.New(backend.Options{
			Name:             name,
			Cfg:              b,
			Network:          policy,
			MaxResponseBytes: cfg.Server.MaxResponseBytes,
			Log:              log,
		})
		if err != nil {
			return nil, fmt.Errorf("backend %q: %w", name, err)
		}
		clients[name] = client
	}

	router, err := routing.New(cfg, func(name string) int {
		c, ok := clients[name]
		if !ok {
			return 0
		}
		return c.Inflight()
	})
	if err != nil {
		return nil, fmt.Errorf("routing: %w", err)
	}

	// Token accounting (PLAN §37-42): a quota tracker and the bounded
	// JSONL writer. When enabled, replay the recent log tail at startup so
	// a daemon restart does not trivially reset per-key token windows
	// (PLAN §40). Startup is fail-closed: an unusable accounting file is
	// a startup error rather than a silent reset.
	var quota *accounting.Quota
	var writer *accounting.Writer
	if cfg.Accounting.Enabled {
		quota = accounting.NewQuota()
		w, err := accounting.NewWriter(accounting.WriterConfig{
			Path:          cfg.Accounting.Path,
			QueueSize:     cfg.Accounting.QueueSize,
			FSync:         cfg.Accounting.FSync,
			FSyncInterval: cfg.Accounting.FSyncInterval.Duration(),
			Log:           log,
		})
		if err != nil {
			return nil, err
		}
		writer = w
		if cfg.Accounting.ReplayOnStart != nil && *cfg.Accounting.ReplayOnStart {
			if err := quota.Replay(cfg.Accounting.Path, cfg.Accounting.ReplayMaxBytes); err != nil {
				_ = writer.Close()
				return nil, fmt.Errorf("accounting replay: %w", err)
			}
		}
		log.Info("accounting enabled",
			"path", cfg.Accounting.Path,
			"fsync", cfg.Accounting.FSync,
			"queue_size", cfg.Accounting.QueueSize,
		)
	}

	prox, err := proxy.New(cfg, router, clients, log, quota, writer)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	api := httpapi.New(cfg, log, store, router, prox)

	log.Info("configuration loaded",
		"backends", len(cfg.Backends),
		"models", len(cfg.Models),
		"keys", len(users.Keys),
		"accounting_enabled", cfg.Accounting.Enabled,
	)
	return &daemon{cfg: cfg, log: log, api: api, proxy: prox, acc: writer, tlsConfig: tlsConfig}, nil
}

// listenAddr returns the listener address for logging/cleanup.
func (d *daemon) listenAddr() string {
	return d.cfg.Server.Listen.Address
}

// isNonLoopbackListenAddr reports whether a TCP listen address is NOT
// loopback (PLAN §8.2).
func isNonLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // fail closed on malformed addresses
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}
