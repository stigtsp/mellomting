package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/agent"
	"mellomting/internal/config"
	"mellomting/internal/inflight"
	"mellomting/internal/topview"
	"mellomting/internal/topview/sim"
	"mellomting/internal/topview/term"
	"mellomting/internal/usagehist"
)

// maxTopUserAgent bounds the user agent column of the legacy table, so
// one client's long string cannot push every other column off the
// screen.
const maxTopUserAgent = 28

// maxStatsReply bounds a /stats reply; the daemon caps it lower.
const maxStatsReply = 4 << 20

// topCmd runs `mellomting top`: the interactive viewer of the daemon's
// admin socket (PLAN §43.2), plain text on a pipe or with --once.
//
// Exit codes: 0 ok, 1 the daemon could not be reached, 2 usage error.
func topCmd(args []string) int {
	fs := commandFlags("top", "Watch what the daemon is doing and what holds it back. Requires server.admin_socket.")
	var configPath string
	var interval time.Duration
	var once, redact, simulate bool
	var rows int
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	fs.DurationVar(&interval, "interval", time.Second, "refresh `INTERVAL`")
	fs.BoolVar(&once, "once", false, "print one snapshot as plain text and exit")
	fs.IntVar(&rows, "rows", 0, "most requests --once prints (default all)")
	fs.BoolVar(&redact, "redact", false, "replace IP addresses with stable labels (toggle with r)")
	fs.BoolVar(&simulate, "simulate", false, "show a made-up busy daemon, to try the view without one")
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: top: unexpected arguments %q\n", fs.Args())
		return 2
	}
	if rows < 0 {
		fmt.Fprintln(os.Stderr, "mellomting: top: --rows must not be negative")
		return 2
	}
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "mellomting: top: --interval must be positive")
		return 2
	}
	opt := topview.Options{Redact: redact, Interval: interval, Rows: rows, NoColor: os.Getenv("NO_COLOR") != ""}

	// SIGHUP too: a closed terminal must still restore and exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	var src topview.Source
	if simulate {
		s := sim.New(1, time.Now(), time.Now)
		src = s
		if path, cleanup, err := simUsage(s); err == nil {
			defer cleanup()
			opt.Usage, _ = usagehist.Open(path, time.Now())
			opt.BackendServer = sim.BackendServer
		}
	} else {
		cfg, err := config.Load(ResolveConfigPath(configPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
			return 1
		}
		sock := cfg.Server.AdminSocket
		if sock == "" {
			fmt.Fprint(os.Stderr, "mellomting: top: add server.admin_socket to your configuration and restart Mellomting:\n\n"+
				"  server:\n    admin_socket: /run/mellomting/admin.sock\n")
			return 1
		}
		src = newAdminSource(unixClient(sock))
		if !once {
			opt.Usage, opt.UsageErr = openUsage(cfg)
			opt.BackendServer = backendServers(cfg)
		}
	}

	if once || !term.IsTerminal(os.Stdout) || !term.IsTerminal(os.Stdin) {
		opt.Color = !opt.NoColor && term.IsTerminal(os.Stdout)
		if once {
			return topOnce(ctx, os.Stdout, os.Stderr, src, opt)
		}
		// Not a terminal: a snapshot per interval, like top -b.
		for {
			if code := topOnce(ctx, os.Stdout, os.Stderr, src, opt); code != 0 {
				return code
			}
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(interval):
			}
			fmt.Fprintln(os.Stdout)
		}
	}

	// Ask once before taking over the terminal, so an old daemon or a
	// missing socket is reported on a normal screen.
	if _, err := src.Stats(ctx, 0); err != nil {
		if errors.Is(err, errOldDaemon) {
			fmt.Fprintln(os.Stderr, "mellomting: top: the daemon is older than this top and serves only the request table; upgrade it for the full view")
			return legacyTop(ctx, src.(*adminSource).client, interval)
		}
		fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
		return 1
	}
	sess, err := term.Start(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
		return 1
	}
	defer sess.Restore()
	opt.In = os.Stdin
	if err := topview.Run(ctx, src, sess, opt); err != nil {
		sess.Restore()
		fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
		return 1
	}
	return 0
}

// topOnce prints one snapshot. Against a daemon older than /stats it
// says so and prints the request table that daemon can serve.
func topOnce(ctx context.Context, w, errw io.Writer, src topview.Source, opt topview.Options) int {
	st, err := src.Stats(ctx, 0)
	if errors.Is(err, errOldDaemon) {
		fmt.Fprintln(errw, "mellomting: top: the daemon is older than this top and serves only the request table; upgrade it for the full view")
		snap, err := fetchInflight(ctx, src.(*adminSource).client)
		if err != nil {
			fmt.Fprintf(errw, "mellomting: top: %v\n", err)
			return 1
		}
		if opt.Redact {
			red := topview.NewRedactor()
			for i := range snap.Requests {
				snap.Requests[i].Remote = red.Apply(snap.Requests[i].Remote)
				snap.Requests[i].UserAgent = red.Apply(snap.Requests[i].UserAgent)
			}
			for i := range snap.Agents {
				snap.Agents[i].Remote = red.Apply(snap.Agents[i].Remote)
			}
		}
		render(w, snap, time.Now(), false, opt.Rows)
		return 0
	}
	if err != nil {
		fmt.Fprintf(errw, "mellomting: top: %v\n", err)
		return 1
	}
	topview.Plain(w, st, opt)
	return 0
}

// legacyTop is the request table, redrawn, for a daemon without /stats.
func legacyTop(ctx context.Context, client *http.Client, interval time.Duration) int {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snap, err := fetchInflight(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
			return 1
		}
		render(os.Stdout, snap, time.Now(), true, 40)
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// errOldDaemon is a daemon that answers /stats with 404.
var errOldDaemon = oldDaemon{}

type oldDaemon struct{}

func (oldDaemon) Error() string { return "the daemon does not serve /stats" }
func (oldDaemon) Banner() string {
	return "the daemon no longer serves /stats (downgraded?)"
}

// adminSource reads GET /stats over the admin socket.
type adminSource struct{ client *http.Client }

func newAdminSource(c *http.Client) *adminSource { return &adminSource{client: c} }

func (a *adminSource) Stats(ctx context.Context, since int64) (adminapi.Stats, error) {
	var st adminapi.Stats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://admin/stats?since="+strconv.FormatInt(since, 10), nil)
	if err != nil {
		return st, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return st, fmt.Errorf("cannot reach the admin socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return st, errOldDaemon
	}
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("admin socket returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatsReply)).Decode(&st); err != nil {
		return st, fmt.Errorf("invalid admin response: %w", err)
	}
	return st, nil
}

// openUsage opens the accounting log for the Usage view, or says why
// it cannot: top runs as the operator, who may not be able to read it.
func openUsage(cfg *config.Config) (*usagehist.History, string) {
	if !cfg.Accounting.Enabled || cfg.Accounting.Path == "" {
		return nil, "accounting is off in this configuration"
	}
	h, err := usagehist.Open(cfg.Accounting.Path, time.Now())
	switch {
	case err == nil:
		return h, ""
	case os.IsPermission(err):
		return nil, "cannot read the accounting log: permission denied (run top as a user who can read it)"
	case os.IsNotExist(err):
		return nil, "the accounting log does not exist yet"
	}
	return nil, "cannot read the accounting log"
}

// backendServers maps each generated backend name to its server, so
// the Usage view lists servers.
func backendServers(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Backends))
	for name, b := range cfg.Backends {
		if b.Server != "" {
			out[name] = b.Server
		}
	}
	return out
}

// simUsage writes the simulator's history to a temporary file.
func simUsage(s *sim.Sim) (string, func(), error) {
	f, err := os.CreateTemp("", "mellomting-sim-usage-*.jsonl")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	err = s.WriteUsage(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// unixClient talks HTTP over the admin socket. The host in the URL is
// ignored by the dialer and exists only because HTTP requires one.
func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func fetchInflight(ctx context.Context, client *http.Client) (adminapi.Snapshot, error) {
	var snap adminapi.Snapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://admin/inflight", nil)
	if err != nil {
		return snap, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return snap, fmt.Errorf("cannot reach the admin socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, fmt.Errorf("admin socket returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&snap); err != nil {
		return snap, fmt.Errorf("invalid admin response: %w", err)
	}
	return snap, nil
}

// render draws one frame. clear is false for a single snapshot, so the
// output can be piped somewhere without escape sequences in it.
func render(w io.Writer, snap adminapi.Snapshot, now time.Time, clear bool, maxRows int) {
	if clear {
		// Home the cursor and erase, rather than scrolling: a redraw
		// that scrolls makes a steady table impossible to read.
		fmt.Fprint(w, "\033[H\033[2J")
	}
	header := fmt.Sprintf("mellomting %s — %d in flight", now.Format(time.TimeOnly), len(snap.Requests))
	if snap.Agents != nil {
		header += fmt.Sprintf(", %s attached", plural(len(snap.Agents), "agent"))
	}
	fmt.Fprintf(w, "%s\n", header)

	// The daemon sends requests oldest first, so a cut keeps the ones
	// worth looking at.
	writeTable(w, requestColumns, snap.Requests, maxRows)
	writeTable(w, agentColumns, snap.Agents, 0)
}

// column is one column of a top table: its title and how to render a
// row's cell.
type column[T any] struct {
	title string
	cell  func(T) string
}

// writeTable renders rows under a blank line, aligned in columns. A
// positive maxRows keeps the table to that many rows and says how many
// were left out, so a live view of hundreds of requests stays on one
// screen. An empty table prints nothing.
func writeTable[T any](w io.Writer, cols []column[T], rows []T, maxRows int) {
	if len(rows) == 0 {
		return
	}
	hidden := 0
	if maxRows > 0 && len(rows) > maxRows {
		rows, hidden = rows[:maxRows], len(rows)-maxRows
	}
	fmt.Fprintln(w)
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = c.title
	}
	fmt.Fprintln(t, strings.Join(cells, "\t"))
	for _, r := range rows {
		for i, c := range cols {
			cells[i] = c.cell(r)
		}
		fmt.Fprintln(t, strings.Join(cells, "\t"))
	}
	t.Flush()
	if hidden > 0 {
		fmt.Fprintf(w, "… and %d more\n", hidden)
	}
}

var requestColumns = []column[inflight.Record]{
	{"AGE", func(r inflight.Record) string { return age(r.AgeMillis) }},
	{"PHASE", func(r inflight.Record) string { return string(r.Phase) }},
	{"KEY", func(r inflight.Record) string { return orDash(r.KeyName) }},
	{"MODEL", func(r inflight.Record) string { return orDash(r.Model) }},
	{"TOKENS", func(r inflight.Record) string { return tokens(r.Tokens) }},
	{"IN", func(r inflight.Record) string { return byteCount(r.BytesIn) }},
	{"OUT", func(r inflight.Record) string { return byteCount(r.BytesOut) }},
	{"CLIENT", func(r inflight.Record) string { return orDash(r.Remote) }},
	{"USER-AGENT", func(r inflight.Record) string { return truncate(r.UserAgent, maxTopUserAgent) }},
	// The agent a request was routed to, so a stuck stream can be tied
	// to the instance serving it.
	{"AGENT", func(r inflight.Record) string {
		if agent.IsBackendName(r.Backend) {
			return r.Backend
		}
		return "-"
	}},
}

// agentColumns describe attached agents (PLAN §104). SLOTS is admitted
// over the agent's max_concurrency; QUEUE waits for a slot; STREAMS are
// responses still open on the agent's connection; FAIL counts requests
// that got no response at all.
var agentColumns = []column[agent.Record]{
	{"AGENT", func(a agent.Record) string { return a.Name }},
	{"KEY", func(a agent.Record) string { return a.KeyName }},
	{"REMOTE", func(a agent.Record) string { return orDash(a.Remote) }},
	{"UP", func(a agent.Record) string { return age(a.AgeMillis) }},
	{"STATE", func(a agent.Record) string { return a.State }},
	{"SLOTS", func(a agent.Record) string { return fmt.Sprintf("%d/%d", a.Admitted, a.MaxConcurrency) }},
	{"QUEUE", func(a agent.Record) string { return strconv.Itoa(a.Queued) }},
	{"STREAMS", func(a agent.Record) string { return strconv.FormatInt(a.Streams, 10) }},
	{"REQS", func(a agent.Record) string { return strconv.FormatInt(a.Requests, 10) }},
	{"FAIL", func(a agent.Record) string { return strconv.FormatInt(a.Failures, 10) }},
	{"MODELS", func(a agent.Record) string {
		names := make([]string, len(a.Models))
		for i, m := range a.Models {
			names[i] = m.Name
		}
		return orDash(strings.Join(names, ","))
	}},
}

// age renders a duration at a width that does not jump around as a
// request ages.
func age(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// tokens renders a count the backend has not reported yet as absent,
// rather than as zero: an unfinished stream has no total to show.
func tokens(n int64) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func byteCount(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate cuts on rune boundaries: a user agent is arbitrary client
// text, and slicing bytes out of a multi-byte one prints a replacement
// character in the middle of the table.
func truncate(s string, n int) string {
	if s == "" {
		return "-"
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
