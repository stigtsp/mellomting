package main

import (
	"context"
	"encoding/json"
	"flag"
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
)

// maxTopUserAgent bounds the user agent column so one client's long
// string cannot push every other column off the screen.
const maxTopUserAgent = 28

// topCmd runs `mellomting top`: a live view of the requests the daemon
// is serving.
//
// Exit codes: 0 ok, 1 the daemon could not be reached, 2 usage error.
func topCmd(args []string) int {
	fs := commandFlags("top", "Watch active requests. Requires server.admin_socket.")
	var configPath string
	var interval time.Duration
	var once bool
	var rows int
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	fs.DurationVar(&interval, "interval", time.Second, "refresh `INTERVAL`")
	fs.BoolVar(&once, "once", false, "print one snapshot and exit")
	fs.IntVar(&rows, "rows", 40, "most requests to show (default 40 live; --once shows all unless given)")
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: top: unexpected arguments %q\n", fs.Args())
		return 2
	}
	if rows < 1 {
		fmt.Fprintln(os.Stderr, "mellomting: top: --rows must be positive")
		return 2
	}
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "mellomting: top: --interval must be positive")
		return 2
	}

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

	client := unixClient(sock)
	if once {
		snap, err := fetchInflight(context.Background(), client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
			return 1
		}
		limit := 0
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "rows" {
				limit = rows
			}
		})
		render(os.Stdout, snap, time.Now(), false, limit)
		return 0
	}

	// Ctrl-C leaves the alternate screen behind it, so the terminal the
	// operator gets back is the one they left.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
		render(os.Stdout, snap, time.Now(), true, rows)
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
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
