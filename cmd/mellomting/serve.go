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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

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
	pepper    []byte             // HMAC pepper, loaded once at startup (PLAN §27)
	tlsConfig *tls.Config        // static listener TLS (PLAN §67); nil when absent
}

// serveCmd runs the proxy daemon.
//
// Exit codes: 0 clean shutdown, 1 startup or shutdown failure,
// 2 usage error.
func serveCmd(args []string) int {
	fs := commandFlags("serve", "Run the proxy until interrupted. Restart to apply configuration changes.")
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
		log.Warn("backend_network.mode is \"any\": outbound connections to inference backends are not restricted to loopback or allow-listed CIDRs")
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

	srv := newHTTPServer(cfg, d.api, d.log)
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

	// Signals: SIGINT/SIGTERM start the graceful drain (PLAN §74).
	// SIGHUP reloads the users file in place (PLAN §30, T-M10) instead of
	// killing the daemon; the default terminate disposition is suppressed
	// while the signal is registered, so SIGHUP can never kill the
	// process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	var received os.Signal
	for received == nil {
		select {
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
// sandboxPolicy builds the post-startup Landlock policy from validated
// configuration (PLAN §58, §60, §62). It is separate from applySandbox
// so what the daemon confines itself to can be asserted on any host,
// not only one whose kernel can enforce it.
func sandboxPolicy(cfg *config.Config) (landlock.Policy, error) {
	ports, err := landlock.BackendPorts(backendBaseURLs(cfg)...)
	if err != nil {
		return landlock.Policy{}, fmt.Errorf("landlock: %w", err)
	}
	pol := landlock.Policy{
		ReadFiles: []string{cfg.Auth.UsersFile},
		// The directory, not just the file: every key mutation renames a
		// new users file over the old one, and a rule bound to the
		// replaced inode would deny the SIGHUP reload that is supposed
		// to apply it (PLAN §30, §58).
		ReadDirs:   []string{filepath.Dir(cfg.Auth.UsersFile)},
		ConnectTCP: ports,
	}
	if cfg.Accounting.Enabled {
		pol.WriteFiles = append(pol.WriteFiles, cfg.Accounting.Path)
	}
	return pol, nil
}

func applySandbox(cfg *config.Config, log *slog.Logger) error {
	l := cfg.Security.Landlock
	if l.Mode == landlock.ModeDisabled {
		log.Info("landlock disabled by configuration")
		return nil
	}
	required := l.Mode == landlock.ModeRequired

	pol, err := sandboxPolicy(cfg)
	if err != nil {
		return err
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
	abi := min(report.KernelABI, landlock.MaxABI)
	// On non-Linux builds Apply always returns a non-nil error by design
	// (PLAN §7 fail-closed), so the nil check is statically "always
	// true" there; staticcheck reports SA4023 only under GOOS=darwin and
	// is scoped to the linux build in the quality gate (see AGENTS.md).
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
	return fmt.Errorf("a per-key token quota is configured in %s, but a request to model(s) %s whose usage the backend does not report would count zero tokens against it, so the quota would never apply; set accounting.unknown_usage_reservation (or, on a generation model, policy.max_output_tokens)", cfg.Auth.UsersFile, strings.Join(uncounted, ", "))
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

	// Before any client, router, or writer is constructed: a
	// configuration that cannot enforce its own quota should be reported
	// without opening resources first.
	if err := checkQuotaEnforceable(cfg, users); err != nil {
		return nil, err
	}
	hasTokenQuota := quotaKeysConfigured(users.Keys)

	policy, err := backend.PolicyFromConfig(cfg.Security.BackendNetwork)
	if err != nil {
		return nil, err
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
	} else if hasTokenQuota {
		log.Warn("accounting disabled; token quotas are enforced in-memory only (windows reset on restart and no usage is recorded)")
	}

	prox, err := proxy.New(cfg, router, clients, log, quota, writer, hasTokenQuota)
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
		log.Warn("users file has no keys; every request will be rejected until a key is added")
	}
	return &daemon{cfg: cfg, log: log, api: api, proxy: prox, acc: writer, pepper: pepper, tlsConfig: tlsConfig}, nil
}

// reloadUsers reloads the users file and atomically swaps the running key
// store (SIGHUP, PLAN §30). Fail closed: on any error the previous store
// stays in effect, so a malformed edit can never widen or empty access.
// The pepper is reused from memory, so the reload needs no file access
// beyond the users file — the only secret read the Landlock policy grants
// after startup (PLAN §30, §58). In-flight requests are unaffected; they
// keep serving against the store they looked up (PLAN §74).
//
// The reload runs the same admission gates startup runs. A key set that
// startup would have refused to serve must not slip in through a SIGHUP:
// a token quota the configuration cannot charge is silently unlimited,
// which is the opposite of what the operator who just wrote it expects.
func (d *daemon) reloadUsers() {
	users, err := auth.LoadUsers(d.cfg.Auth.UsersFile)
	if err != nil {
		d.logReloadFailed(err)
		return
	}
	if err := checkQuotaEnforceable(d.cfg, users); err != nil {
		d.logReloadFailed(err)
		return
	}
	store, err := auth.NewStore(users, d.pepper)
	if err != nil {
		d.logReloadFailed(err)
		return
	}
	d.api.ReloadStore(store)
	d.log.Info("users reloaded", "keys", len(users.Keys))
	if len(users.Keys) == 0 {
		d.log.Warn("users file has no keys; every request will be rejected until a key is added")
	}
}

// logReloadFailed reports a failed SIGHUP reload (PLAN §30). Fail closed:
// the previous store stays in effect.
//
// The sandbox no longer explains a failure here. It used to: the policy
// granted the users file by pathname, every key mutation renamed a new
// inode over it, and the reload was denied — so the ERROR named the
// sandbox and told the operator to restart. The policy now grants the
// directory (PLAN §58), so a denial is no longer the expected outcome
// and the error carries whatever actually went wrong: a malformed edit,
// a file moved out of the granted directory, or a permission the
// service account does not have.
func (d *daemon) logReloadFailed(err error) {
	d.log.Error("users reload failed; keeping previous store", "error_class", "configuration", "error", err)
}
