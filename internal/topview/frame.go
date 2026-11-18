package topview

import (
	"fmt"
	"strings"
	"time"

	"mellomting/internal/inflight"
	"mellomting/internal/stats"
	"mellomting/internal/topview/term"
)

// The smallest terminal top lays out in; below it, it says so.
const (
	minW = 80
	minH = 24
)

var (
	borderSt = term.Style{FG: term.Ansi256(240)}
	titleSt  = term.Style{FG: term.Ansi256(75), Bold: true}
	headSt   = term.Style{FG: term.Ansi256(245), Bold: true}
	selSt    = term.Style{BG: term.Ansi256(237), Bold: true}
	tabSt    = term.Style{FG: term.Ansi256(252)}
	tabOnSt  = term.Style{FG: term.Ansi256(16), BG: term.Ansi256(75), Bold: true}
	bannerSt = term.Style{FG: term.Ansi256(231), BG: term.Ansi256(160), Bold: true}
)

type frame struct {
	s    *term.Screen
	m    *Model
	ui   *UIState
	w, h int
}

// Frame draws one screen of the model as the UI state asks. It reads
// nothing but its arguments, so the same inputs give the same frame.
func Frame(m *Model, ui UIState, w, h int) *term.Screen {
	s := term.NewScreen(w, h)
	s.NoColor = ui.NoColor
	ui.Height = h
	f := &frame{s: s, m: m, ui: &ui, w: w, h: h}
	if w < minW || h < minH {
		msg := fmt.Sprintf("terminal too small: %d×%d, top needs %d×%d", w, h, minW, minH)
		f.s.Text(max((w-term.Width(msg))/2, 0), h/2, term.Truncate(msg, w), warn)
		return s
	}
	top := f.header()
	switch ui.View {
	case Overview:
		f.overview(top)
	default:
		f.table(top)
	}
	f.banner(top)
	return s
}

// put draws text at (x, y) padded or cut to exactly w cells. Text from
// outside is sanitized and, when on, redacted on the way.
func (f *frame) put(x, y, w int, s string, st term.Style, right bool) {
	if w <= 0 {
		return
	}
	s = f.m.show(f.ui, s)
	if right {
		s = term.PadLeft(s, w)
	} else {
		s = term.Pad(s, w)
	}
	f.s.Text(x, y, s, st)
}

// box draws a panel with a title set into its top border.
func (f *frame) box(x, y, w, h int, title string) {
	f.s.Box(x, y, w, h, "", borderSt)
	if title != "" && w > 8 {
		f.s.Set(x+1, y, '┤', borderSt)
		t := term.Truncate(f.m.show(f.ui, title), w-6)
		n := f.s.Text(x+2, y, t, titleSt)
		f.s.Set(x+2+n, y, '├', borderSt)
	}
}

// header draws the top box: title, clock and view tabs in its border,
// the throughput graphs inside. It returns the first row below it.
func (f *frame) header() int {
	gh := 1
	if f.h >= 34 {
		gh = 2
	}
	hh := 2 + 2*gh + 1
	f.s.Box(0, 0, f.w, hh, "", borderSt)
	m := f.m

	// Title: name, clock, uptime and flags.
	x := 1
	seg := func(s string, st term.Style) {
		x += f.s.Text(x, 0, s, st)
	}
	seg("┤", borderSt)
	seg(" mellomting ", titleSt)
	if m.have {
		seg(m.now().Format("15:04:05"), tabSt)
		if m.St.StartedMillis > 0 {
			seg(" up "+dur(m.now().Sub(time.UnixMilli(m.St.StartedMillis))), dim)
		}
	}
	seg(" ", tabSt)
	if f.ui.Redact {
		seg("REDACTED", term.Style{FG: term.Ansi256(16), BG: term.Ansi256(214), Bold: true})
		seg(" ", tabSt)
	}
	if f.ui.Paused {
		seg("PAUSED", term.Style{Reverse: true, Bold: true})
		seg(" ", tabSt)
	}
	if m.St.Stats.Truncated {
		seg("truncated", warn)
		seg(" ", tabSt)
	}
	seg("├", borderSt)

	// Tabs, right-aligned; a narrow terminal shows only the current one.
	var tabs []string
	for v := Overview; v < numViews; v++ {
		tabs = append(tabs, fmt.Sprintf(" %d %s ", v+1, v))
	}
	full := 0
	for _, t := range tabs {
		full += term.Width(t)
	}
	if x+full+3 < f.w {
		tx := f.w - 2 - full
		f.s.Set(tx-1, 0, '┤', borderSt)
		for v, t := range tabs {
			st := tabSt
			if View(v) == f.ui.View {
				st = tabOnSt
			}
			tx += f.s.Text(tx, 0, t, st)
		}
		f.s.Set(tx, 0, '├', borderSt)
	} else {
		t := fmt.Sprintf(" %d %s ", f.ui.View+1, f.ui.View)
		tx := f.w - 2 - term.Width(t) - 6
		f.s.Text(tx, 0, "┤ 1-6", borderSt)
		f.s.Text(tx+5, 0, t, tabOnSt)
		f.s.Set(tx+5+term.Width(t), 0, '├', borderSt)
	}

	g := &m.global
	pw := (f.w - 2) / 2
	reason, _ := m.recentRefusal(10)
	compRate, offRate := m.rate(g, completed), m.rate(g, offered)
	full5 := func(fn func(b *stats.BucketExport) float64) []float64 { return g.values(m.seq, window, fn) }
	f.panel(1, 1, pw, gh, "req/s", num(compRate), "offered "+num(offRate)+" · err "+num(m.rate(g, failed)), full5(completed), 0, num)
	f.panel(1+pw, 1, f.w-2-pw, gh, "tok/s out", num(m.rate(g, tokOut)), "in "+num(m.rate(g, tokIn))+"/s", full5(tokOut), term.Ansi256(141), num)
	f.panel(1, 1+gh, pw, gh, "net in", size(m.rate(g, bytesIn))+"/s", "out "+size(m.rate(g, bytesOut))+"/s",
		full5(func(b *stats.BucketExport) float64 { return bytesIn(b) + bytesOut(b) }), term.Ansi256(75), size)
	refSub := "nothing refused"
	if reason != "" {
		refSub = "← " + reason
	}
	f.panel(1+pw, 1+gh, f.w-2-pw, gh, "refused/s", num(m.rate(g, refused)), refSub, full5(refused), term.Ansi256(203), num)

	// The summary line names what the graphs cannot.
	var b strings.Builder
	if reason != "" {
		fmt.Fprintf(&b, "refused %s/s mostly by %s · ", num(m.rate(g, refused)), limitName(reason))
	}
	fmt.Fprintf(&b, "inflight %d/%d · offered %s/s · completed %s/s · errors %s/s",
		m.St.Global.Inflight, m.St.Global.MaxInflight, num(offRate), num(compRate), num(m.rate(g, failed)))
	if !m.St.Global.Accounting.Enabled {
		b.WriteString(" · accounting off")
	}
	f.put(2, 1+2*gh, f.w-4, b.String(), dim, false)
	return hh
}

// panel is one header graph: label and current value on the left, the
// last five minutes as braille on the right. color 0 uses the load
// gradient by height.
func (f *frame) panel(x, y, w, gh int, label, value, sub string, vals []float64, color term.Color, peakFmt func(float64) string) {
	lw := 24
	if w < 56 {
		lw = 19
	}
	f.put(x+1, y, lw-2-term.Width(value), label, term.Style{Bold: true}, false)
	f.put(x+lw-1-term.Width(value), y, term.Width(value), value, term.Style{Bold: true, FG: term.Ansi256(231)}, true)
	if gh >= 2 {
		f.put(x+1, y+1, lw-2, sub, dim, false)
	}
	gw := w - lw - 1
	if gw < 4 {
		return
	}
	var peak float64
	for _, v := range vals {
		peak = max(peak, v)
	}
	lines := term.Braille(downsample(vals, 2*gw), gw, gh, peak)
	for r, line := range lines {
		c := color
		if c == 0 {
			c = term.Gradient(0.15 + 0.6*float64(gh-r-1)/float64(max(gh, 1)))
		}
		f.s.Text(x+lw, y+r, line, term.Style{FG: c})
	}
	if peak > 0 {
		p := "↑" + peakFmt(peak)
		f.s.Text(x+w-1-term.Width(p), y, p, dim)
	}
}

// hints writes the keys into the bottom border, dropping the least
// needed ones on a narrow terminal; quitting is always shown.
func (f *frame) hints() {
	items := []string{"↑↓ select", "⏎ detail", "esc close", "←→ 1-6 view", "s sort", "/ filter", "r redact", "p pause", "q quit"}
	switch f.ui.View {
	case Overview:
		items = []string{"↑↓ select", "⏎ detail", "esc close", "←→ 1-6 view", "r redact", "p pause", "q quit"}
	case Usage:
		items[4] = "s keys/models/servers"
	}
	if f.ui.Filtering {
		items = []string{"filter: " + f.ui.Filter + "▏", "⏎ done", "esc clear", "^C quit"}
	}
	for len(items) > 1 {
		s := strings.Join(items, "  ")
		if term.Width(s)+6 <= f.w {
			break
		}
		items = append(items[:len(items)-2], items[len(items)-1]) // drop the one before quit
	}
	s := strings.Join(items, "  ")
	f.s.Set(1, f.h-1, '┤', borderSt)
	n := f.s.Text(2, f.h-1, " "+term.Truncate(f.m.show(f.ui, s), f.w-6)+" ", tabSt)
	f.s.Set(2+n, f.h-1, '├', borderSt)
}

// banner says the daemon is unreachable, over the header's bottom
// border, while the last data stays on screen.
func (f *frame) banner(top int) {
	var msg string
	switch {
	case f.m.err != "":
		msg = " disconnected: " + f.m.err + " · retrying "
	case !f.m.have:
		msg = " connecting… "
	default:
		return
	}
	msg = term.Truncate(msg, f.w-4)
	f.s.Text((f.w-term.Width(msg))/2, top-1, msg, bannerSt)
}

// overview is the dashboard: attention, the busiest items, and either
// the live requests or the selected item's detail.
func (f *frame) overview(top int) {
	m := f.m
	y := top
	att := attention(m)
	limit := 2
	if f.h >= 34 {
		limit = 5
	}
	n := min(len(att), limit)
	title := "Attention"
	if more := len(att) - n; more > 0 {
		title = fmt.Sprintf("Attention · %d more", more)
	}
	f.box(0, y, f.w, n+2, title)
	for i := range n {
		f.s.Text(2, y+1+i, "●", term.Style{FG: att[i].st.FG})
		st := att[i].st
		st.Dim = false
		f.put(4, y+1+i, f.w-6, att[i].text, term.Style{FG: st.FG, Bold: i == 0 && att[i].score > 0}, false)
	}
	y += n + 2

	rows := overviewRows(m, f.ui)
	sel := selIndex(rows, f.ui.Sel[Overview])
	top3 := topN(f.h)
	th := top3 + 3
	bw := f.w / 3
	groups := []struct {
		title, kind string
		x, w        int
		cols        []col
	}{
		{"Keys", "key", 0, bw, []col{{title: "KEY", w: 10, flex: true}, {title: "INFLIGHT", w: 10}, {title: "REQ/S", w: 6, right: true, drop: 2}, {title: "LAST 5M", w: 8, drop: 1}}},
		{"Servers", "server", bw, bw, []col{{title: "SERVER", w: 10, flex: true}, {title: "SLOTS", w: 11}, {title: "ENGINE", w: 9, right: true, drop: 1}}},
		{"Models", "model", 2 * bw, f.w - 2*bw, []col{{title: "MODEL", w: 10, flex: true}, {title: "REQ/S", w: 6, right: true}, {title: "TOK/S", w: 6, right: true, drop: 2}, {title: "LAST 5M", w: 8, drop: 1}}},
	}
	for _, g := range groups {
		f.box(g.x, y, g.w, th, g.title)
		var grows []row
		gsel := -1
		for i, r := range rows {
			if r.kind != g.kind {
				continue
			}
			if i == sel {
				gsel = len(grows)
			}
			r.cells = overviewCells(m, g.kind, r)
			grows = append(grows, r)
		}
		f.grid(g.x+1, y+1, g.w-2, th-2, g.cols, grows, gsel)
	}
	y += th

	if f.ui.Detail && len(rows) > 0 {
		f.detail(rows[sel], 0, y, f.w, f.h-y)
	} else {
		f.live(0, y, f.w, f.h-y)
	}
	f.hints()
}

// overviewCells picks a dashboard row's four cells from its view row.
func overviewCells(m *Model, kind string, r row) []cell {
	switch kind {
	case "key":
		return []cell{r.cells[0], r.cells[1], r.cells[2], r.cells[8]}
	case "server":
		s := m.server(strings.TrimPrefix(r.id, "server:"))
		eng := styled("–", dim)
		if s != nil && s.metrics != nil {
			if s.metrics.HasQueue {
				st := term.Style{}
				if s.metrics.Waiting > 0 {
					st = warn
				}
				eng = styled(fmt.Sprintf("%.0fr %.0fw", s.metrics.Running, s.metrics.Waiting), st)
			} else {
				eng = txt(fmt.Sprintf("%d loaded", len(s.metrics.Loaded)))
			}
		}
		if s != nil {
			if d, _ := cooldown(*s, m.now()); d > 0 {
				eng = styled("cooldown", bad)
			}
		}
		return []cell{r.cells[0], r.cells[2], eng, txt("")}
	case "model":
		return []cell{r.cells[0], r.cells[1], r.cells[2], r.cells[8]}
	}
	return nil
}

// live is the dashboard's bottom box: requests by phase, then the
// oldest live requests.
func (f *frame) live(x, y, w, h int) {
	reqs := f.m.St.Requests
	f.box(x, y, w, h, fmt.Sprintf("Live · %d in flight", len(reqs)))
	counts := map[inflight.Phase]int{}
	for _, r := range reqs {
		counts[r.Phase]++
	}
	var b strings.Builder
	for i, p := range []inflight.Phase{inflight.PhaseReading, inflight.PhaseRouting, inflight.PhaseQueued, inflight.PhaseWaiting, inflight.PhaseStreaming, inflight.PhaseSending} {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s %d", p, counts[p])
	}
	if len(reqs) > 0 {
		fmt.Fprintf(&b, " · oldest %s (%s)", dur(ms(reqs[0].AgeMillis)), reqs[0].KeyName)
	}
	f.put(x+2, y+1, w-4, b.String(), term.Style{}, false)
	cols := []col{{title: "KEY", w: 18}, {title: "MODEL", w: 24, flex: true}, {title: "SERVER", w: 14}, {title: "PHASE", w: 9},
		{title: "AGE", w: 7, right: true}, {title: "TTFB", w: 6, right: true, drop: 2}, {title: "REMOTE", w: 22, drop: 1}}
	rows := requestRows(f.m)
	for i := range rows {
		c := rows[i].cells
		rows[i].cells = []cell{c[0], c[2], c[3], c[4], c[5], c[7], c[1]}
	}
	f.grid(x+1, y+2, w-2, h-3, cols, rows, -1)
}

// table is a list view: the table, and the detail box when open.
func (f *frame) table(top int) {
	rows := viewRows(f.m, f.ui)
	sel := selIndex(rows, f.ui.Sel[f.ui.View])
	avail := f.h - top
	dh := 0
	if f.ui.Detail && len(rows) > 0 {
		dh = min(max(avail/2, 8), 14)
		dh = min(dh, avail-6)
	}
	title := f.tableTitle(len(rows))
	f.box(0, top, f.w, avail-dh, title)
	if f.ui.View == Usage && f.m.usage == nil {
		why := f.m.usageErr
		if why == "" {
			why = "accounting history is not available"
		}
		f.put(2, top+2, f.w-4, why, warn, false)
	} else {
		f.grid(1, top+1, f.w-2, avail-dh-2, columns(f.ui.View), rows, sel)
	}
	if dh > 0 {
		f.detail(rows[sel], 0, f.h-dh, f.w, dh)
	}
	f.hints()
}

func (f *frame) tableTitle(n int) string {
	v := f.ui.View
	t := fmt.Sprintf("%s · %d", v, n)
	if v == Usage {
		t = fmt.Sprintf("Usage · %s", usageDims[f.ui.UsageDim])
		if f.m.usage != nil {
			from, to := f.m.usage.Covers()
			if !from.IsZero() {
				t += fmt.Sprintf(" · %s – %s", from.In(f.m.Loc).Format("Jan 2 15:04"), to.In(f.m.Loc).Format("Jan 2 15:04"))
			}
			if f.m.usage.Truncated() {
				t += " · oldest cut"
			}
		}
	}
	if modes := sortModes[v]; len(modes) > 0 {
		t += " · sort " + modes[f.ui.Sort[v]%len(modes)]
	}
	if f.ui.Filter != "" || f.ui.Filtering {
		t += " · filter " + f.ui.Filter
	}
	return t
}

// layout fits columns into w cells: it drops columns in their drop
// order until the rest fit, and gives the flexible column what is left.
func layout(cols []col, w int) ([]col, []int) {
	keep := append([]col(nil), cols...)
	width := func(cs []col) int {
		n := 2 // selection marker
		for _, c := range cs {
			n += c.w + 1
		}
		return n
	}
	for width(keep) > w {
		worst := -1
		for i, c := range keep {
			if c.drop > 0 && (worst < 0 || c.drop < keep[worst].drop) {
				worst = i
			}
		}
		if worst < 0 {
			break
		}
		keep = append(keep[:worst], keep[worst+1:]...)
	}
	ws := make([]int, len(keep))
	extra := w - width(keep)
	for i, c := range keep {
		ws[i] = c.w
		if c.flex && extra > 0 {
			ws[i] += extra
			extra = 0
		}
	}
	// Still too wide (no droppable columns left): shrink from the right.
	for over := -extra; over > 0; {
		for i := len(ws) - 1; i >= 0 && over > 0; i-- {
			if ws[i] > 4 {
				ws[i]--
				over--
			}
		}
	}
	return keep, ws
}

// grid draws a header and as many rows as fit, scrolled so the
// selected row (sel; -1 for none) is visible.
func (f *frame) grid(x, y, w, h int, cols []col, rows []row, sel int) {
	if h < 2 {
		return
	}
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[c.title] = i
	}
	keep, ws := layout(cols, w)
	cx := x + 2
	for i, c := range keep {
		f.put(cx, y, ws[i], c.title, headSt, c.right)
		cx += ws[i] + 1
	}
	vis := h - 1
	off := 0
	if sel >= vis {
		off = sel - vis + 1
	}
	for line := 0; line < vis && off+line < len(rows); line++ {
		i := off + line
		r := rows[i]
		ry := y + 1 + line
		st := term.Style{}
		if i == sel {
			f.s.Fill(x, ry, w, 1, ' ', selSt)
			f.s.Text(x, ry, "▶", term.Style{FG: term.Ansi256(75), BG: selSt.BG, Bold: true})
			st = selSt
		}
		cx := x + 2
		for k, c := range keep {
			if j := idx[c.title]; j < len(r.cells) {
				f.cell(cx, ry, ws[k], r.cells[j], st, c.right)
			}
			cx += ws[k] + 1
		}
	}
	if len(rows) == 0 {
		msg := "nothing to show"
		if f.ui.Filter != "" {
			msg = "nothing matches the filter"
		}
		f.put(x+2, y+1, w-4, msg, dim, false)
	}
}

// cell draws one table cell: a graph, a meter with its text, or text.
func (f *frame) cell(x, y, w int, c cell, row term.Style, right bool) {
	if w <= 0 {
		return
	}
	base := c.st
	if row.BG != 0 {
		base.BG, base.Bold = row.BG, base.Bold || row.Bold
	}
	if c.graph != nil {
		line := term.Braille(downsample(c.graph, 2*w), w, 1, c.gtop)[0]
		f.s.Text(x, y, line, term.Style{FG: c.color, BG: row.BG})
		return
	}
	if c.meter >= 0 && w >= 8 {
		mw := min(5, w-term.Width(c.text)-1)
		if mw >= 2 {
			bar, color := term.Meter(c.meter, mw)
			filled := strings.Count(bar, "■")
			f.s.Text(x, y, strings.Repeat("■", filled), term.Style{FG: color, BG: row.BG})
			f.s.Text(x+filled, y, strings.Repeat("·", mw-filled), term.Style{FG: term.Ansi256(238), BG: row.BG})
			x, w = x+mw+1, w-mw-1
		}
	}
	f.put(x, y, w, c.text, base, right)
}
