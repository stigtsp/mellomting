// Command serve: run the Mellomting proxy daemon (PLAN §92).
//
// Startup is fail-closed: configuration, the key store, the pepper,
// backend URL/validation, and (when required) the sandbox gate must all
// succeed before the listener accepts traffic. On SIGINT/SIGTERM the
// daemon performs the graceful shutdown sequence (PLAN §74).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
	"mellomting/internal/version"
)

// daemon holds the fully-wired components of one Mellomting instance.
type daemon struct {
	cfg    *config.Config
	log    *slog.Logger
	api    *httpapi.Server
	proxy  *proxy.Proxy
	acc    *accounting.Writer // usage JSONL writer; nil when disabled
	listen net.Listener
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

	d, err := buildDaemon(cfg, log)
	if err != nil {
		log.Error("startup failed", "error_class", "startup")
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	// Sandbox gate (PLAN §55, §57): mode required is fail-closed. Policy
	// application itself lands in the hardening phase (PLAN §95); until
	// then a required sandbox means "cannot claim the safety property",
	// which is a startup failure rather than a degradation.
	if err := gateSandbox(cfg.Security.Landlock, log); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	ln, err := buildListener(cfg.Server.Listen)
	if err != nil {
		log.Error("listener failed", "error_class", "listener", "network", cfg.Server.Listen.Network)
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}
	d.listen = ln
	defer os.Remove(d.listenAddr()) // best-effort socket cleanup

	if cfg.Server.TLS.Mode == "" && cfg.Server.Listen.Network == "tcp" &&
		isNonLoopbackListenAddr(cfg.Server.Listen.Address) &&
		cfg.Server.AllowPlaintextNonLoopback {
		log.Warn("plaintext non-loopback TCP listener is active (PLAN §8.2)")
	}

	srv := newHTTPServer(cfg, d.api)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	d.api.SetReady(true)
	log.Info("mellomting ready",
		"version", version.String(),
		"network", cfg.Server.Listen.Network,
		"address", cfg.Server.Listen.Address,
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

// gateSandbox enforces the configured Landlock policy at startup.
func gateSandbox(l config.Landlock, log *slog.Logger) error {
	report := landlock.Check()
	switch l.Mode {
	case "disabled":
		log.Info("landlock disabled by configuration")
		return nil
	case "best-effort":
		if !report.Supported {
			log.Warn("landlock not applied (unavailable)", "reason", report.Reason)
			return nil
		}
		log.Warn("landlock not applied yet; enforcement lands in the hardening phase (PLAN §95)",
			"kernel_abi", report.KernelABI)
		return nil
	default: // "required"
		return errors.New("security.landlock.mode is required but sandbox enforcement is not yet available (PLAN §55, §95); set mode to best-effort or disabled for this phase")
	}
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
// socket, and applies the configured mode after bind.
func safeUnixListen(l config.Listen) (net.Listener, error) {
	mode, err := strconv.ParseUint(l.Mode, 8, 16)
	if err != nil || mode == 0 || mode > 0o777 {
		return nil, fmt.Errorf("unix socket mode %q must be octal (e.g. 0660)", l.Mode)
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
// reading the key store, pepper, and backend credential files.
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
	return &daemon{cfg: cfg, log: log, api: api, proxy: prox, acc: writer}, nil
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
