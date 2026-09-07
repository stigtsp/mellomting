package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"mellomting/internal/adminapi"
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
	fs := commandFlags("top", "Watch the requests the daemon is serving.\nRequires server.admin_socket in the configuration.")
	var configPath string
	var interval time.Duration
	var once bool
	fs.StringVar(&configPath, "config", "", configFlagHelp)
	fs.DurationVar(&interval, "interval", time.Second, "refresh `INTERVAL`")
	fs.BoolVar(&once, "once", false, "print one snapshot and exit")
	if err := parseCommandFlags(fs, args); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: top: unexpected arguments %q\n", fs.Args())
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
		fmt.Fprint(os.Stderr, "mellomting: top: the daemon publishes no live view; add an admin socket to the configuration:\n\n"+
			"  server:\n    admin_socket: /run/mellomting/admin.sock\n\n"+
			"and restart it — the socket is bound at startup.\n")
		return 1
	}

	client := unixClient(sock)
	if once {
		records, err := fetchInflight(context.Background(), client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
			return 1
		}
		render(os.Stdout, records, time.Now(), false)
		return 0
	}

	// Ctrl-C leaves the alternate screen behind it, so the terminal the
	// operator gets back is the one they left.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		records, err := fetchInflight(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintf(os.Stderr, "mellomting: top: %v\n", err)
			return 1
		}
		render(os.Stdout, records, time.Now(), true)
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

func fetchInflight(ctx context.Context, client *http.Client) ([]inflight.Record, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://admin/inflight", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the daemon is not answering on its admin socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin socket returned %s", resp.Status)
	}
	var snap adminapi.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&snap); err != nil {
		return nil, fmt.Errorf("admin socket returned something unreadable: %w", err)
	}
	return snap.Requests, nil
}

// render draws one frame. clear is false for a single snapshot, so the
// output can be piped somewhere without escape sequences in it.
func render(w io.Writer, records []inflight.Record, now time.Time, clear bool) {
	if clear {
		// Home the cursor and erase, rather than scrolling: a redraw
		// that scrolls makes a steady table impossible to read.
		fmt.Fprint(w, "\033[H\033[2J")
	}
	fmt.Fprintf(w, "mellomting %s — %d in flight\n\n", now.Format(time.TimeOnly), len(records))
	if len(records) == 0 {
		fmt.Fprintln(w, "no requests in flight")
		return
	}
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "AGE\tPHASE\tKEY\tMODEL\tTOKENS\tIN\tOUT\tCLIENT\tUSER-AGENT")
	for _, r := range records {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			age(r.AgeMillis), r.Phase, orDash(r.KeyName), orDash(r.Model),
			tokens(r.Tokens), byteCount(r.BytesIn), byteCount(r.BytesOut),
			orDash(r.Remote), truncate(r.UserAgent, maxTopUserAgent))
	}
	t.Flush()
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
