package topview

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/topview/sim"
	"mellomting/internal/topview/term"
	"mellomting/internal/usagehist"
)

var update = flag.Bool("update", false, "rewrite golden frames")

var simStart = time.Date(2026, 11, 18, 13, 23, 34, 0, time.UTC)

func simModel(t *testing.T) (*Model, adminapi.Stats) {
	t.Helper()
	st := sim.New(42, simStart, nil).Snapshot(0)
	m := NewModel(time.UTC)
	if m.Apply(st) {
		t.Fatal("first apply reported a reset")
	}
	return m, st
}

func lines(s *term.Screen) []string { return strings.Split(strings.TrimRight(s.String(), "\n"), "\n") }

func TestGoldenFrames(t *testing.T) {
	m, _ := simModel(t)
	for _, size := range [][2]int{{120, 40}, {80, 24}} {
		for v := Overview; v < numViews; v++ {
			ui := NewUI(Options{})
			ui.View = v
			if v == Keys {
				ui.Detail = true // the detail box, on the selected (first) key
			}
			got := Frame(m, ui, size[0], size[1]).String()
			name := filepath.Join("testdata", strings.ToLower(v.String())+"_"+itoa(size[0])+"x"+itoa(size[1])+".golden")
			if *update {
				os.MkdirAll("testdata", 0o755)
				os.WriteFile(name, []byte(got), 0o644)
				continue
			}
			want, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("%s: %v (run with -update after checking the frame)", name, err)
			}
			if got != string(want) {
				t.Errorf("%s differs:\n%s", name, got)
			}
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func usageLine(at time.Time, key, model, backend string, charged int64) string {
	return fmt.Sprintf(`{"time":%q,"key_id":%q,"model":%q,"backend":%q,"input_tokens":%d,"output_tokens":0,"usage_status":"exact","charged_tokens":%d}`+"\n",
		at.Format(time.RFC3339Nano), key, model, backend, charged, charged)
}

func writeUsage(t *testing.T, lines []string) *usagehist.History {
	t.Helper()
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := usagehist.Open(p, simStart)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestStartsOnOverview(t *testing.T) {
	m, _ := simModel(t)
	ui := NewUI(Options{})
	if ui.View != Overview {
		t.Fatalf("starts on %v", ui.View)
	}
	out := Frame(m, ui, 120, 40).String()
	for _, want := range []string{"Attention", "coding_agents", "agent-lab-6"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
}

// The screenshot's two blind spots read as the first things to look at.
func TestAttentionRanking(t *testing.T) {
	m, st := simModel(t)
	st.Global.Accounting.Dropped = 12
	m.St = st
	got := attention(m)
	if len(got) < 4 {
		t.Fatalf("attention %q", texts(got))
	}
	order := []string{"accounting dropped 12", "gpu2 in cooldown", "coding_agents refused", "agent-lab-6 vLLM queueing"}
	at := 0
	for _, a := range got {
		if at < len(order) && strings.Contains(a.text, order[at]) {
			at++
		}
	}
	if at != len(order) {
		t.Fatalf("attention order %q, want %q in that order", texts(got), order)
	}
	if !strings.Contains(texts(got)[2], "concurrent_requests (8)") {
		t.Errorf("the refusal does not name its limit: %q", texts(got)[2])
	}

	quiet := NewModel(time.UTC)
	quiet.Apply(adminapi.Stats{NowMillis: simStart.UnixMilli(), StartedMillis: 1})
	if a := attention(quiet); len(a) != 1 || a[0].text != "nothing held back" {
		t.Fatalf("quiet attention %q", texts(a))
	}
}

func texts(a []attn) []string {
	var out []string
	for _, x := range a {
		out = append(out, x.text)
	}
	return out
}

func TestSelectionFollowsID(t *testing.T) {
	m, st := simModel(t)
	ui := NewUI(Options{})
	ui.View = Keys
	rows := viewRows(m, &ui)
	ui.Sel[Keys] = rows[1].id
	chosen := rows[1].id
	// Reverse the sort: the chosen key moves, the selection stays on it.
	ui.Sort[Keys] = len(sortModes[Keys]) - 1
	m.Apply(st)
	after := viewRows(m, &ui)
	if after[selIndex(after, ui.Sel[Keys])].id != chosen {
		t.Fatalf("selection lost: %q", ui.Sel[Keys])
	}
	ui.handle(term.Key{Code: term.Down}, m)
	if ui.Sel[Keys] == chosen {
		t.Fatal("down did not move")
	}
	ui.handle(term.Key{Code: term.Home}, m)
	if ui.Sel[Keys] != after[0].id {
		t.Fatal("home did not go to the first row")
	}
	out := Frame(m, ui, 120, 40).String()
	if !strings.Contains(out, "▶") {
		t.Fatal("no selection marker")
	}
}

func TestFilterAndSort(t *testing.T) {
	m, _ := simModel(t)
	ui := NewUI(Options{})
	ui.View = Keys
	for _, k := range []term.Key{{Code: term.Rune, Rune: '/'}, {Code: term.Rune, Rune: 'd'}, {Code: term.Rune, Rune: 'o'}, {Code: term.Enter}} {
		ui.handle(k, m)
	}
	rows := viewRows(m, &ui)
	if len(rows) != 1 || rows[0].name != "doc_indexer" {
		t.Fatalf("filtered rows %v", rowNames(rows))
	}
	ui.handle(term.Key{Code: term.Esc}, m)
	if ui.Filter != "" {
		t.Fatal("esc did not clear the filter")
	}
	ui.Sort[Keys] = sortIndex(Keys, "name")
	rows = viewRows(m, &ui)
	for i := 1; i < len(rows); i++ {
		if rows[i-1].name > rows[i].name {
			t.Fatalf("not sorted by name: %v", rowNames(rows))
		}
	}
	before := ui.Sort[Keys]
	ui.handle(term.Key{Code: term.Rune, Rune: 's'}, m)
	if ui.Sort[Keys] == before {
		t.Fatal("s did not change the sort")
	}
}

func rowNames(rows []row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

func TestRedactStableLabels(t *testing.T) {
	r := NewRedactor()
	a := r.Apply("from 192.0.2.10:4431 via [2001:db8::17]:443")
	b := r.Apply("again 192.0.2.10 and 2001:db8::17")
	if strings.Contains(a+b, "192.0.2") || strings.Contains(a+b, "2001:db8") {
		t.Fatalf("address survived: %q %q", a, b)
	}
	if a != "from ip-1:4431 via [ip-2]:443" || b != "again ip-1 and ip-2" {
		t.Fatalf("labels not stable: %q %q", a, b)
	}
	if got := r.Apply("version 1.2.3 and 999.1.1.1 and a::b::c"); got != "version 1.2.3 and 999.1.1.1 and a::b::c" {
		t.Fatalf("non-addresses rewritten: %q", got)
	}
	m, _ := simModel(t)
	ui := NewUI(Options{Redact: true})
	ui.View = Requests
	out := Frame(m, ui, 160, 50).String()
	if strings.Contains(out, "192.0.2.") || strings.Contains(out, "198.51.100.") || strings.Contains(out, "2001:db8") || strings.Contains(out, "203.0.113") {
		t.Fatalf("redacted frame shows an address:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED") || !strings.Contains(out, "ip-") {
		t.Fatal("no REDACTED marker or labels")
	}
}

func TestTooSmall(t *testing.T) {
	m, _ := simModel(t)
	out := Frame(m, NewUI(Options{}), 60, 20).String()
	if !strings.Contains(out, "too small") {
		t.Fatalf("frame %q", out)
	}
}

func TestResizeRelayout(t *testing.T) {
	m, _ := simModel(t)
	for _, size := range [][2]int{{80, 24}, {120, 40}, {97, 31}, {200, 60}} {
		w, h := size[0], size[1]
		for v := Overview; v < numViews; v++ {
			for _, detail := range []bool{false, true} {
				ui := NewUI(Options{})
				ui.View, ui.Detail = v, detail
				s := Frame(m, ui, w, h)
				ls := lines(s)
				for i, l := range ls {
					if term.Width(l) > w {
						t.Fatalf("%v %dx%d line %d is %d wide", v, w, h, i, term.Width(l))
					}
				}
				if len(ls) != h || !strings.HasSuffix(ls[h-1], "╯") || !strings.Contains(ls[h-1], "q quit") {
					t.Fatalf("%v %dx%d detail=%v: bottom border %q", v, w, h, detail, ls[len(ls)-1])
				}
			}
		}
	}
}

type stubSource struct {
	replies []adminapi.Stats
	errs    []error
	since   []int64
}

func (s *stubSource) Stats(_ context.Context, since int64) (adminapi.Stats, error) {
	s.since = append(s.since, since)
	i := len(s.since) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return adminapi.Stats{}, s.errs[i]
	}
	return s.replies[min(i, len(s.replies)-1)], nil
}

func TestSeqResetRefetches(t *testing.T) {
	first := sim.New(1, simStart, nil).Snapshot(0)
	lower := first
	lower.Stats.Seq -= 100
	restarted := sim.New(1, simStart.Add(time.Minute), nil).Snapshot(0)
	restarted.StartedMillis += 5000
	src := &stubSource{replies: []adminapi.Stats{first, lower, first, restarted, restarted}}
	m := NewModel(time.UTC)
	ctx := context.Background()
	for range 5 {
		poll(ctx, src, m)
	}
	want := []int64{0, first.Stats.Seq, 0, first.Stats.Seq, 0}
	for i, s := range src.since {
		if s != want[i] {
			t.Fatalf("since sequence %v, want %v", src.since, want)
		}
	}
}

func TestDisconnectBanner(t *testing.T) {
	first := sim.New(1, simStart, nil).Snapshot(0)
	src := &stubSource{replies: []adminapi.Stats{first}, errs: []error{nil, errors.New("dial unix /run/mellomting/admin.sock: connect: connection refused")}}
	m := NewModel(time.UTC)
	ctx := context.Background()
	poll(ctx, src, m)
	poll(ctx, src, m)
	out := Frame(m, NewUI(Options{}), 120, 40).String()
	if !strings.Contains(out, "disconnected") || !strings.Contains(out, "coding_agents") {
		t.Fatalf("banner or last data missing:\n%s", out)
	}
	if strings.Contains(out, "/run/mellomting") {
		t.Fatal("banner shows the error text verbatim")
	}
	poll(ctx, src, m)
	if len(src.since) != 3 || src.since[2] != first.Stats.Seq {
		t.Fatalf("did not retry from the last seq: %v", src.since)
	}
	if strings.Contains(Frame(m, NewUI(Options{}), 120, 40).String(), "disconnected") {
		t.Fatal("banner stayed after reconnecting")
	}
}

func TestPlainNoEscapes(t *testing.T) {
	_, st := simModel(t)
	st.Requests[0].UserAgent = "evil\x1b[31m/1.0"
	st.Keys[0].Name = "key\x1b]0;pwned\x07name"
	var b bytes.Buffer
	Plain(&b, st, Options{Redact: true})
	out := b.String()
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
		t.Fatalf("escape in plain output: %q", out)
	}
	for _, want := range []string{"evil/1.0", "coding_agents", "agent-lab-6", "REQUESTS"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output lacks %q", want)
		}
	}
	if strings.Contains(out, "192.0.2.") {
		t.Fatal("plain --redact shows an address")
	}
}

func TestUsageMapsBackendsToServers(t *testing.T) {
	m, _ := simModel(t)
	h := writeUsage(t, []string{
		usageLine(simStart.Add(-time.Hour), "k-agents", "chat-fast", "auto-gpu1-aaaaaaaaaaaa", 100),
		usageLine(simStart.Add(-time.Hour), "k-agents", "chat-large", "auto-gpu1-bbbbbbbbbbbb", 50),
		usageLine(simStart.Add(-2*time.Hour), "k-index", "chat-large", "agent-lab-6", 7),
	})
	m.SetUsage(h, "", map[string]string{"auto-gpu1-aaaaaaaaaaaa": "gpu1", "auto-gpu1-bbbbbbbbbbbb": "gpu1"})
	ui := NewUI(Options{})
	ui.View = Usage
	ui.UsageDim = 2
	rows := viewRows(m, &ui)
	if len(rows) != 2 || rows[0].name != "gpu1" || rows[1].name != "agent-lab" {
		t.Fatalf("usage rows %v", rowNames(rows))
	}
	ui.UsageDim = 0
	rows = viewRows(m, &ui)
	if rows[0].name != "coding_agents" {
		t.Fatalf("key id not resolved to its name: %v", rowNames(rows))
	}
}

// hangingSource never answers until ctx ends, like a stuck daemon.
type hangingSource struct{ calls chan struct{} }

func (h hangingSource) Stats(ctx context.Context, _ int64) (adminapi.Stats, error) {
	h.calls <- struct{}{}
	<-ctx.Done()
	return adminapi.Stats{}, ctx.Err()
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A key press redraws at once even while the daemon does not answer,
// and q quits.
func TestRunInputIndependentOfPoll(t *testing.T) {
	inR, inW := io.Pipe()
	out := &syncBuf{}
	src := hangingSource{calls: make(chan struct{}, 4)}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), src, term.FakeSession(out, 100, 30), Options{In: inR, Interval: time.Hour})
	}()
	<-src.calls // the poll is now stuck
	inW.Write([]byte("2"))
	deadline := time.After(2 * time.Second)
	for !strings.Contains(out.String(), "Keys ·") {
		select {
		case <-deadline:
			t.Fatal("the key press did not redraw while the poll hung")
		case <-time.After(5 * time.Millisecond):
		}
	}
	inW.Write([]byte("q"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("q did not quit")
	}
}

// Colour on a terminal marks headings and attention only; the tables
// carry none, so they still align.
func TestPlainColorIsSparing(t *testing.T) {
	_, st := simModel(t)
	var b bytes.Buffer
	Plain(&b, st, Options{Color: true})
	out := b.String()
	if !strings.Contains(out, "\x1b[1mATTENTION\x1b[0m") || !strings.Contains(out, "\x1b[38;5;203m●") {
		t.Fatalf("no colour on headings or attention:\n%q", out)
	}
	tables := out[strings.Index(out, "KEYS"):]
	// The KEYS heading's reset, then BACKENDS, MODELS and REQUESTS.
	if strings.Count(tables, "\x1b[") != 1+2*3 {
		t.Fatalf("colour inside tables:\n%q", tables)
	}
}
