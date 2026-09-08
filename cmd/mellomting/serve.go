// Command serve: run the Mellomting proxy daemon (PLAN §92).
//
// Startup is fail-closed: configuration, the key store, the pepper,
// backend URL/validation, and (when required) the sandbox gate must all
// succeed before the listener accepts traffic. On SIGINT/SIGTERM the
// daemon performs the graceful shutdown sequence (PLAN §74).
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/adminapi"
	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/inflight"
	"mellomting/internal/landlock"
	"mellomting/internal/logging"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
	"mellomting/internal/sandbox"
	"mellomting/internal/seatbelt"
	"mellomting/internal/securefile"
	"mellomting/internal/tlsconfig"
	"mellomting/internal/version"
)

// adminSocketMode keeps the live-request view to the service account
// and root: it shows every caller's address, user agent and token use,
// which is not the ingress's audience.
const adminSocketMode = "0600"

// listenerAddrs is the set of bound sockets the sandbox policy must
// keep open.
func listenerAddrs(lns ...net.Listener) []net.Addr {
	addrs := make([]net.Addr, 0, len(lns))
	for _, ln := range lns {
		if ln != nil {
			addrs = append(addrs, ln.Addr())
		}
	}
	return addrs
}

// daemon holds the fully-wired components of one Mellomting instance.
type daemon struct {
	usersHash       [sha256.Size]byte // last observed users-file content
	usersReadFailed bool              // suppress repeated polling read errors
	cfg             *config.Config
	log             *slog.Logger
	api             *httpapi.Server
	proxy           *proxy.Proxy
	acc             *accounting.Writer // usage JSONL writer; nil when disabled
	live            *inflight.Registry // in-flight requests, for `mellomting top`
	pepper          []byte             // HMAC pepper, loaded once at startup (PLAN §27)
	tlsConfig       *tls.Config        // static listener TLS (PLAN §67); nil when absent
}

// serveCmd runs the proxy daemon.
//
// Exit codes: 0 clean shutdown, 1 startup or shutdown failure,
// 2 usage error.
func serveCmd(args []string) int {
	fs := commandFlags("serve", "Run the proxy until interrupted. Configuration changes need a restart;\nkey changes apply on reload (systemctl reload, or SIGHUP).")
	var configPath string
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: serve: unexpected arguments %q\n", fs.Args())
		return 2
	}
	resolved := ResolveConfigPath(configPath)

	cfg, err := config.Load(resolved)
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

	ln, err := buildListener(cfg.Server.Listen)
	if err != nil {
		log.Error("listener failed", "error_class", "listener", "network", cfg.Server.Listen.Network)
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}
	// The admin socket is bound here too, before the sandbox: the
	// policy has to name every listener the daemon keeps answering on.
	var adminLn net.Listener
	if p := cfg.Server.AdminSocket; p != "" {
		adminLn, err = buildListener(config.Listen{Network: "unix", Address: p, Mode: adminSocketMode})
		if err != nil {
			log.Error("admin listener failed", "error_class", "listener")
			fmt.Fprintf(os.Stderr, "mellomting: serve: admin socket: %v\n", err)
			_ = ln.Close()
			return 1
		}
	}
	// Static TLS (PLAN §67): wrap the listener before the sandbox is
	// applied. The certificate and key were already loaded with the rest
	// of the startup secrets (PLAN §57 step 9).
	if d.tlsConfig != nil {
		ln = tls.NewListener(ln, d.tlsConfig)
	}
	// FIX-05/M19: there is deliberately NO deferred os.Remove of the Unix
	// socket path here. Go's net.UnixListener already unlinks the socket
	// when the listener is closed, and Shutdown/Close close it at the
	// start of the grace period (T-L11's liveness check at serve.go:396
	// stays). A deferred os.Remove fires only after serve() returns — up
	// to grace_period later — against whatever then owns the path,
	// unlinking a start-before-stop / blue-green replacement's fresh
	// socket and taking it off the path while its process keeps running.

	if !config.TLSConfigured(cfg.Server.TLS) && cfg.Server.Listen.Network == "tcp" &&
		config.IsNonLoopbackListenAddress(cfg.Server.Listen.Address) &&
		cfg.Server.AllowPlaintextNonLoopback {
		log.Warn("plaintext non-loopback TCP listener is active")
	}

	if cfg.Security.BackendNetwork.Mode == "any" {
		log.Warn("backend_network.mode is \"any\": backend addresses are unrestricted")
	}

	// Sandbox confinement (PLAN §53-63, §95). This is the last step
	// before the listener accepts (PLAN §57 step 19): the policy is
	// applied to every runtime thread and verified before any client
	// request may be processed. No config/secret file descriptors are
	// open here (auth.LoadUsers/LoadPepper and securefile.Read close
	// their own FDs), so the open-file caveat (PLAN §59) is satisfied.
	if err := applySandbox(cfg, listenerAddrs(ln, adminLn), log); err != nil {
		log.Error("sandbox enforcement failed", "error_class", "sandbox")
		_ = ln.Close()
		if adminLn != nil {
			_ = adminLn.Close()
		}
		fmt.Fprintf(os.Stderr, "mellomting: serve: %v\n", err)
		return 1
	}

	srv := newHTTPServer(cfg, d.api, d.log)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	if adminLn != nil {
		// Its own server: a slow or wedged admin reader must not touch
		// the ingress, and the admin socket carries no client traffic
		// to bound.
		adminSrv := &http.Server{
			Handler:           adminapi.Handler(d.live),
			ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		}
		go func() { _ = adminSrv.Serve(adminLn) }()
		defer func() { _ = adminSrv.Close() }()
		log.Info("admin socket ready", "address", cfg.Server.AdminSocket)
	}

	d.api.SetReady(true)
	log.Info("mellomting ready",
		"version", version.String(),
		"network", cfg.Server.Listen.Network,
		"address", cfg.Server.Listen.Address,
		"tls", d.tlsConfig != nil,
		"backends", len(cfg.Backends),
		"models", len(cfg.Models),
	)

	// Signals: SIGINT/SIGTERM start the graceful drain (PLAN §74).
	// SIGHUP reloads the users file in place (PLAN §30, T-M10) instead of
	// killing the daemon; the default terminate disposition is suppressed
	// while the signal is registered, so SIGHUP can never kill the
	// process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	usersPoll := time.NewTicker(time.Second)
	defer usersPoll.Stop()

	var received os.Signal
	for received == nil {
		select {
		case <-usersPoll.C:
			d.refreshUsers(false)
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("server stopped", "error_class", "server")
				return 1
			}
			return 0
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				d.reloadUsers()
				continue
			}
			received = sig
		}
	}

	log.Info("shutting down", "grace_period", cfg.Shutdown.GracePeriod.Duration().String())
	// PLAN §74: 1-2. stop accepting, reject new admissions.
	d.proxy.BeginDraining()
	// 3. let active streams finish up to the deadline. A second
	// SIGINT/SIGTERM/SIGHUP during the drain forces a prompt exit with an
	// accounting flush instead of being swallowed (T-M11): the drain
	// either completes on its own or the second signal severs remaining
	// handlers.
	timeout := cfg.Shutdown.GracePeriod.Duration()
	gctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		_ = srv.Shutdown(gctx)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case sig := <-sigCh:
		log.Warn("second signal during drain; forcing shutdown", "signal", sig.String())
		// 4. cancel whatever remains.
		srv.Close()
		<-shutdownDone
	}
	// 7. close listener. Shutdown already closed it on the clean path,
	// so "use of closed network connection" is expected, not a WARN
	// (T-L12).
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Warn("listener close", "error", err)
	}
	// 8. flush and close the accounting writer (PLAN §42). The final-sync
	// error (e.g. ENOSPC) is surfaced here (T-M11), never discarded.
	if d.acc != nil {
		if err := d.acc.Close(); err != nil {
			log.Error("accounting final-sync failed", "error", err)
		}
	}
	log.Info("shutdown complete")
	return 0
}

// newDaemonLogger builds the structured logger per PLAN §43.
func newDaemonLogger(cfg *config.Config) (*slog.Logger, error) {
	return logging.New(os.Stdout, cfg.Logging.Level, cfg.Logging.Format)
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
func applySandbox(cfg *config.Config, listeners []net.Addr, log *slog.Logger) error {
	return enforceSandbox(platformSandbox(cfg), cfg, listeners, log)
}

// enforceSandbox applies one backend's policy under the configured mode.
// The mode semantics — disabled does nothing, best-effort warns, required
// refuses to start — are the same whichever backend enforces them, so
// they live here and are exercised against a stub rather than by
// confining the test process irreversibly.
func enforceSandbox(b sandboxBackend, cfg *config.Config, listeners []net.Addr, log *slog.Logger) error {
	if b.mode == sandbox.ModeDisabled {
		log.Info("sandbox disabled by configuration", "backend", b.name)
		return nil
	}

	pol, err := sandboxPolicy(cfg, listeners)
	if err != nil {
		return err
	}

	failClosed := func(reason, detail string) error {
		if b.mode == sandbox.ModeRequired {
			msg := reason
			if detail != "" {
				msg += ": " + detail
			}
			return fmt.Errorf("security.%s.mode is %q but the sandbox cannot be enforced: %s", b.name, b.mode, msg)
		}
		log.Warn("running without a sandbox", "backend", b.name, "reason", reason, "detail", detail)
		return nil
	}

	report := b.check()
	if !report.Supported {
		return failClosed("sandbox cannot be enforced", report.Reason)
	}
	if reason, detail := b.ready(report); reason != "" {
		return failClosed(reason, detail)
	}
	if err := b.apply(report, pol); err != nil {
		return failClosed("sandbox application failed", err.Error())
	}
	fields := append([]any{"backend", b.name, "mode", b.mode}, b.applied(report)...)
	log.Info("sandbox enforced", append(fields, "rules", pol.Summarize())...)
	return nil
}

// sandboxBackend is one platform's enforcement mechanism, bound to the
// configuration section that governs it. Only the backend for the
// running platform is ever consulted, so a configuration may carry both
// and be served anywhere (PLAN §55).
type sandboxBackend struct {
	name  string
	mode  string
	check func() sandbox.Report
	// ready reports why an available sandbox still must not be used,
	// or "" when it is ready. It is where a backend puts the version
	// floor its own mechanism has.
	ready func(sandbox.Report) (reason, detail string)
	apply func(sandbox.Report, sandbox.Policy) error
	// applied names what was enforced, for the operational log.
	applied func(sandbox.Report) []any
}

func platformSandbox(cfg *config.Config) sandboxBackend {
	if runtime.GOOS == "darwin" {
		return sandboxBackend{
			name:    "seatbelt",
			mode:    cfg.Security.Seatbelt.Mode,
			check:   seatbelt.Check,
			ready:   func(sandbox.Report) (string, string) { return "", "" },
			apply:   func(_ sandbox.Report, p sandbox.Policy) error { return seatbelt.Apply(p) },
			applied: func(sandbox.Report) []any { return nil },
		}
	}
	// Landlock is the backend everywhere else, including platforms with
	// no sandbox at all: Check reports it unavailable there and the mode
	// decides whether that is fatal.
	l := cfg.Security.Landlock
	return sandboxBackend{
		name:  "landlock",
		mode:  l.Mode,
		check: landlock.Check,
		ready: func(r sandbox.Report) (string, string) {
			if r.KernelABI < l.MinimumABI {
				return "kernel Landlock ABI is below the configured minimum",
					fmt.Sprintf("kernel ABI %d < minimum_abi %d", r.KernelABI, l.MinimumABI)
			}
			return "", ""
		},
		// Enforce at the highest ABI supported by both the kernel and
		// the pinned library (PLAN §55 step 3).
		//
		// On non-Linux builds Apply always returns a non-nil error by
		// design (PLAN §7 fail-closed), so the nil check is statically
		// "always true" there; staticcheck reports SA4023 only under
		// GOOS=darwin and is scoped to the linux build in the quality
		// gate (see AGENTS.md).
		apply: func(r sandbox.Report, p sandbox.Policy) error {
			return landlock.Apply(min(r.KernelABI, landlock.MaxABI), p)
		},
		applied: func(r sandbox.Report) []any {
			return []any{"kernel_abi", r.KernelABI, "applied_abi", min(r.KernelABI, landlock.MaxABI)}
		},
	}
}

// checkQuotaEnforceable fails closed when the configuration would leave a
// per-key token quota unenforceable (PLAN §39). It is a pure function of
// the configuration and the key store, so it runs before any client,
// writer, or listener is built — both because a configuration error
// should be reported before resources are opened, and because it is then
// testable without them.
func checkQuotaEnforceable(cfg *config.Config, users *auth.UsersFile) error {
	hasQuota := quotaKeysConfigured(users.Keys)

	// FIX-04/N10: ensure_stream_usage and unknown_usage_reservation are
	// only meaningful when a per-key token quota is in effect. With
	// accounting disabled and no quota anywhere, accepting them would be
	// a silent no-op.
	if !cfg.Accounting.Enabled && !hasQuota &&
		(cfg.Accounting.EnsureStreamUsage != nil || cfg.Accounting.UnknownUsageReservation != 0) {
		return fmt.Errorf("accounting.ensure_stream_usage/unknown_usage_reservation require a per-key token quota (tokens_per_hour or tokens_per_day) when accounting is disabled; no key in %s carries one", cfg.Auth.UsersFile)
	}

	// A quota is only enforceable if a request whose usage the backend
	// never reports still counts against it. The fallback amount is
	// unknown_usage_reservation, or the model's output cap on a
	// generation model; with both zero such a request counts zero, the
	// quota never advances, and the limit is silently unlimited. An
	// embedding model has no output cap to fall back to, so only the
	// reservation can account for it.
	if !hasQuota || cfg.Accounting.UnknownUsageReservation != 0 {
		return nil
	}
	var uncounted []string
	for name, m := range cfg.Models {
		if m.Type == "embedding" || m.Policy.MaxOutputTokens == 0 {
			uncounted = append(uncounted, name)
		}
	}
	if len(uncounted) == 0 {
		return nil
	}
	slices.Sort(uncounted)
	return fmt.Errorf("token quotas need a fallback for missing usage on %s (%s); set accounting.unknown_usage_reservation or, for generation models, policy.max_output_tokens", plural(len(uncounted), "model"), strings.Join(uncounted, ", "))
}

// quotaKeysConfigured reports whether any key carries a token budget, so
// the daemon can warn when accounting is disabled but quotas would
// otherwise apply (T-M5).
func quotaKeysConfigured(keys []auth.Key) bool {
	for _, k := range keys {
		if k.Limits.TokensPerHour > 0 || k.Limits.TokensPerDay > 0 {
			return true
		}
	}
	return false
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

// isSocketLive reports whether a live listener is accepting on the Unix
// socket path: a successful dial means an owner is bound to it (T-L9). A
// stale socket (no listener behind it) refuses the dial.
func isSocketLive(addr string) bool {
	conn, err := net.DialTimeout("unix", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
			return nil, fmt.Errorf("refusing to unlink %q: it is a symlink", l.Address)
		}
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to unlink %q: it is not a unix socket", l.Address)
		}
		// Liveness check (T-L9): refuse to steal a socket a live
		// listener is accepting on. Only a stale socket (nothing
		// behind it, so a dial is refused) is removed.
		if isSocketLive(l.Address) {
			return nil, fmt.Errorf("refusing to unlink %q: a live listener is accepting on it (T-L9)", l.Address)
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
func newHTTPServer(cfg *config.Config, api *httpapi.Server, log *slog.Logger) *http.Server {
	// Bounded accepted connections (T-L15, PLAN §9.1): connections above
	// the configured cap are closed at accept time so a flood of idle
	// sockets cannot exhaust fds. StateClosed and StateHijacked both
	// release a slot; net/http fires StateHijacked as soon as a handler
	// hijacks the connection (SSE streaming), so a hijacked connection no
	// longer counts against the cap.
	var active atomic.Int64
	max := int64(cfg.Server.MaxConnections)
	srv := &http.Server{
		Handler: api.Handler(),
		// Route net/http's own error output (TLS handshake failures,
		// unexpected handler panics) through the structured pipeline so
		// no plaintext message bypasses it (T-L10).
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		ReadTimeout:       cfg.Server.ReadHeaderTimeout.Duration() + cfg.Server.ReadBodyTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
	}
	srv.ConnState = func(c net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			// If the counter exceeds the cap, close the connection. We
			// do NOT compensate here: net/http still fires StateClosed
			// for this connection once its serve loop ends, which is
			// what releases the slot. Compensating here too would
			// double-decrement and let the effective cap drift upward.
			if active.Add(1) > max {
				_ = c.Close()
			}
		case http.StateClosed, http.StateHijacked:
			active.Add(-1)
		}
	}
	return srv
}

// sandboxPolicy builds the post-startup policy from validated
// configuration and the bound listener (PLAN §58, §60, §62). It is
// backend-neutral, and separate from applySandbox so what the daemon
// confines itself to can be asserted on any host, not only one that
// can enforce it.
func sandboxPolicy(cfg *config.Config, listeners []net.Addr) (sandbox.Policy, error) {
	ports, err := sandbox.BackendPorts(backendBaseURLs(cfg)...)
	if err != nil {
		return sandbox.Policy{}, fmt.Errorf("sandbox: %w", err)
	}
	pol := sandbox.Policy{
		// The users file's directory rather than the file: a Landlock
		// rule binds to the inode behind the path, and every key
		// mutation renames a new users file over the old one, so a rule
		// on the file stops matching at the first `key create` and the
		// SIGHUP reload meant to apply it is denied. The grant is
		// read-only, but the directory normally also holds the pepper
		// and the configuration, both already read at startup; an
		// operator who wants the narrower grant gives the users file a
		// directory of its own (PLAN §30, §58).
		ReadPaths:  []string{filepath.Dir(cfg.Auth.UsersFile)},
		ConnectTCP: ports,
	}
	if cfg.Accounting.Enabled {
		pol.WriteFiles = append(pol.WriteFiles, cfg.Accounting.Path)
	}
	// The listener is already bound; a backend that filters accepts has
	// to be told to keep it open. It is read from the listener itself
	// rather than the configuration, because the two differ whenever
	// the operator asked the kernel to choose: an address ending in :0
	// is a real port by now, and naming the configured 0 would grant
	// nothing and leave the daemon unable to answer.
	for _, a := range listeners {
		switch addr := a.(type) {
		case *net.UnixAddr:
			pol.Listeners = append(pol.Listeners, sandbox.Listener{UnixPath: addr.Name})
		case *net.TCPAddr:
			pol.Listeners = append(pol.Listeners, sandbox.Listener{TCPPort: uint16(addr.Port)})
		}
	}
	return pol, nil
}

// buildDaemon wires the key store, router, backend clients, proxy, and
// HTTP surface from validated configuration. It performs no I/O beyond
// reading the key store, pepper, backend credentials, and the static TLS
// certificate and key (PLAN §57 step 9); all of those FDs are closed
// before the sandbox is applied.
func buildDaemon(cfg *config.Config, log *slog.Logger) (*daemon, error) {
	pepper, err := auth.LoadPepper(cfg.Auth.PepperFile)
	if err != nil {
		return nil, fmt.Errorf("load pepper: %w", err)
	}
	// Before any client, router, or writer is constructed: a key set
	// the daemon cannot serve should be reported without opening
	// resources first.
	data, err := securefile.Read(cfg.Auth.UsersFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("load users file: %w", err)
	}
	store, users, err := parseStore(cfg, pepper, data)
	if err != nil {
		return nil, err
	}

	policy, err := backend.PolicyFromConfig(cfg.Security.BackendNetwork)
	if err != nil {
		return nil, err
	}

	// The trust store is read here, with the rest of the startup
	// secrets and before the sandbox: crypto/x509 would otherwise read
	// it on the first https handshake, which happens after confinement,
	// and the policy grants no path to it (PLAN §57 step 9, §58).
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system trust store: %w", err)
	}

	// Static TLS (PLAN §57 step 9, §67): the certificate and key are
	// loaded once, before the sandbox, and are reloaded only by a process
	// restart in v1.
	var tlsConfig *tls.Config
	if config.TLSConfigured(cfg.Server.TLS) {
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
			RootCAs:          roots,
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

	// Token accounting (PLAN §37-42). Token quotas are always enforced
	// (T-M5): the quota tracker is in-memory and independent of JSONL
	// persistence, so accounting.enabled:false can no longer silently
	// void per-key token budgets. accounting.enabled only controls
	// whether usage records are written and replayed at startup. Startup
	// is fail-closed: an unusable accounting file is a startup error
	// rather than a silent reset.

	quota := accounting.NewQuota()
	var writer *accounting.Writer
	if cfg.Accounting.Enabled {
		w, err := accounting.NewWriter(accounting.WriterConfig{
			Path:          cfg.Accounting.Path,
			QueueSize:     cfg.Accounting.QueueSize,
			FSync:         cfg.Accounting.FSync,
			FSyncInterval: cfg.Accounting.FSyncInterval.Duration(),
			Overflow:      cfg.Accounting.Overflow,
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
	} else if quotaKeysConfigured(users.Keys) {
		log.Warn("accounting disabled; usage is not recorded and token quotas reset on restart")
	}

	// Requests are tracked only when something can read the view: with
	// no admin socket the registry would record work nobody observes.
	var live *inflight.Registry
	if cfg.Server.AdminSocket != "" {
		live = inflight.New()
	}
	prox, err := proxy.New(cfg, router, clients, log, quota, writer, live)
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
	if len(users.Keys) == 0 {
		log.Warn("no API keys configured; add one with mellomting key create")
	}
	return &daemon{cfg: cfg, log: log, api: api, proxy: prox, acc: writer, live: live, pepper: pepper, tlsConfig: tlsConfig, usersHash: sha256.Sum256(data)}, nil
}

// parseStore validates users-file bytes and builds the key store the daemon
// will serve from. It is the one admission path for a key set, at
// startup and on every SIGHUP reload alike, so a key set startup would
// refuse — a token quota the configuration cannot enforce, which would
// be silently unlimited — cannot slip in through a reload either.
func parseStore(cfg *config.Config, pepper, data []byte) (*auth.Store, *auth.UsersFile, error) {
	users, err := auth.ParseUsers(data)
	if err != nil {
		return nil, nil, fmt.Errorf("load users file: %w", err)
	}
	if err := checkQuotaEnforceable(cfg, users); err != nil {
		return nil, nil, err
	}
	store, err := auth.NewStore(users, pepper)
	if err != nil {
		return nil, nil, fmt.Errorf("key store: %w", err)
	}
	return store, users, nil
}

// reloadUsers reloads the users file and atomically swaps the running key
// store (SIGHUP, PLAN §30). Fail closed: on any error the previous store
// stays in effect, so a malformed edit can never widen or empty access.
// The pepper is reused from memory, so the reload needs no file access
// beyond the users file's directory, the only read the Landlock policy
// grants after startup (PLAN §58). In-flight requests are unaffected;
// they keep serving against the store they looked up (PLAN §74).
func (d *daemon) reloadUsers() {
	d.refreshUsers(true)
}

// refreshUsers runs on the signal loop, serializing polling with SIGHUP.
// Hash the bounded, securely opened bytes rather than metadata so atomic
// replacements and same-size edits are detected. Never publish invalid data.
func (d *daemon) refreshUsers(force bool) {
	data, err := securefile.Read(d.cfg.Auth.UsersFile, 1<<20)
	if err != nil {
		if force || !d.usersReadFailed {
			d.log.Error("users reload failed; keeping previous store", "error_class", "users_read")
		}
		d.usersReadFailed = true
		return
	}
	d.usersReadFailed = false
	hash := sha256.Sum256(data)
	if !force && hash == d.usersHash {
		return
	}
	// Remember rejected content too: retry when it changes, without logging
	// the same malformed file every second. SIGHUP always retries explicitly.
	d.usersHash = hash
	store, users, err := parseStore(d.cfg, d.pepper, data)
	if err != nil {
		d.log.Error("users reload failed; keeping previous store", "error_class", "configuration", "error", err)
		return
	}
	d.api.ReloadStore(store)
	d.log.Info("users reloaded", "keys", len(users.Keys))
	if len(users.Keys) == 0 {
		d.log.Warn("no API keys configured; add one with mellomting key create")
	}
}
