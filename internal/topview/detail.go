package topview

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"mellomting/internal/topview/term"
	"mellomting/internal/usagehist"
)

// line is one line of a detail box: segments with their own styles.
type line []seg

type seg struct {
	text string
	st   term.Style
}

func plain(format string, args ...any) line {
	return line{{fmt.Sprintf(format, args...), term.Style{}}}
}

// graph is one detail graph.
type graph struct {
	label string
	vals  []float64
	color term.Color
}

// detail draws the selected item's box: what limits it, what it is
// doing, what it relates to; and its own graphs on the right.
func (f *frame) detail(r row, x, y, w, h int) {
	kind, id := r.kind, strings.TrimPrefix(r.id, r.kind+":")
	f.box(x, y, w, h, fmt.Sprintf("%s %s", kind, r.name))
	var lines []line
	var graphs []graph
	switch kind {
	case "key":
		lines, graphs = f.keyDetail(id)
	case "server":
		lines, graphs = f.serverDetail(id)
	case "model":
		lines, graphs = f.modelDetail(id)
	case "request":
		lines = f.requestDetail(id)
	case "usage":
		lines, graphs = f.usageDetail(id)
	}
	inner := h - 2
	tw := w - 4
	gw := 0
	if len(graphs) > 0 && w >= 100 {
		gw = w * 2 / 5
		tw = w - 4 - gw - 2
	} else if len(graphs) > 0 && kind == "usage" {
		gw, tw = w-4, 0 // the hourly chart is the detail
	}
	if tw > 0 {
		for i, l := range lines {
			if i >= inner {
				break
			}
			lx := x + 2
			for _, s := range l {
				left := x + 2 + tw - lx
				if left <= 0 {
					break
				}
				t := term.Truncate(f.m.show(f.ui, s.text), left)
				lx += f.s.Text(lx, y+1+i, t, s.st)
			}
		}
	}
	if gw > 0 {
		gx := x + w - 2 - gw
		if tw == 0 {
			gx = x + 2
		}
		f.graphs(gx, y+1, gw, inner, graphs)
	}
}

// graphs stacks labelled braille graphs in a region.
func (f *frame) graphs(x, y, w, h int, gs []graph) {
	n := len(gs)
	if n == 0 || h < 2 {
		return
	}
	each := h / n
	if each < 2 {
		n, each = h/2, 2
	}
	for i := range n {
		g := gs[i]
		gy := y + i*each
		var peak float64
		for _, v := range g.vals {
			peak = max(peak, v)
		}
		f.put(x, gy, w, fmt.Sprintf("%s  peak %s", g.label, num(peak)), dim, false)
		for r, l := range term.Braille(downsample(g.vals, 2*w), w, each-1, peak) {
			f.s.Text(x, gy+1+r, l, term.Style{FG: g.color})
		}
	}
}

func refusalsLine(label string, m map[string]int64) line {
	if len(m) == 0 {
		return line{{label + " none", dim}}
	}
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int { return cmp.Compare(m[b], m[a]) })
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return line{{label + " ", dim}, {strings.Join(parts, ", "), bad}}
}

func (f *frame) keyDetail(id string) ([]line, []graph) {
	m := f.m
	h := m.get("key", id)
	var out []line
	if k := m.keyInfo(id); k != nil {
		l := line{}
		if k.ConcurrentRequests > 0 {
			st := term.Style{}
			if k.Inflight >= k.ConcurrentRequests {
				st = bad
			}
			l = append(l, seg{"concurrent_requests ", dim}, seg{fmt.Sprintf("%d/%d in flight", k.Inflight, k.ConcurrentRequests), st})
			if frac, ok := m.atLimit(h); ok {
				l = append(l, seg{fmt.Sprintf(" (at limit %.0f%% of 5m)", 100*frac), dim})
			}
		} else {
			l = append(l, seg{fmt.Sprintf("%d in flight, no concurrent_requests limit", k.Inflight), dim})
		}
		out = append(out, l)
		if k.RequestsPerSecond > 0 {
			out = append(out, line{{"requests_per_second ", dim}, {fmt.Sprintf("%s burst %d", num(k.RequestsPerSecond), k.Burst), term.Style{}}})
		}
		if k.TokensPerHour > 0 || k.TokensPerDay > 0 {
			q := line{{"quota ", dim}}
			if k.TokensPerHour > 0 {
				q = append(q, seg{fmt.Sprintf("hour %s/%s resets %s", num(float64(k.HourUsed)), num(float64(k.TokensPerHour)),
					time.UnixMilli(k.HourResetsMillis).In(m.Loc).Format("15:04")), term.Style{}})
			}
			if k.TokensPerDay > 0 {
				q = append(q, seg{fmt.Sprintf(" · day %s/%s", num(float64(k.DayUsed)), num(float64(k.TokensPerDay))), term.Style{}})
			}
			out = append(out, q)
		}
	}
	out = append(out, plain("req/s %s · tok/s %s · refused %s/min", num(m.rate(h, completed)), num(m.rate(h, tokAll)), num(m.perMin(h, refused))))
	if h != nil {
		out = append(out, refusalsLine("refused 5m:", h.refusals), refusalsLine("errors 5m:", h.errors))
	}
	models, servers := map[string]int{}, map[string]int{}
	for _, r := range m.St.Requests {
		if r.KeyID == id {
			models[r.Model]++
			servers[r.Backend]++
		}
	}
	out = append(out, line{{"live models ", dim}, {counted(models), term.Style{}}}, line{{"live servers ", dim}, {counted(servers), term.Style{}}})
	return out, []graph{{"offered/s", m.spark(h, window, offered), term.Ansi256(75)}, {"refused/s", m.spark(h, window, refused), term.Ansi256(203)}}
}

func counted(m map[string]int) string {
	if len(m) == 0 {
		return "none in flight"
	}
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int {
		if c := cmp.Compare(m[b], m[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%d)", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func (f *frame) serverDetail(name string) ([]line, []graph) {
	m := f.m
	s := m.server(name)
	if s == nil {
		return []line{plain("gone")}, nil
	}
	h := m.get("server", name)
	var out []line
	slots := line{{"slots ", dim}, {fmt.Sprintf("%d/%d admitted", s.admitted, s.max), term.Style{}}}
	if s.queueMax > 0 || s.queued > 0 {
		slots = append(slots, seg{" · queue ", dim}, seg{fmt.Sprintf("%d/%d", s.queued, s.queueMax), term.Style{}})
	}
	slots = append(slots, seg{fmt.Sprintf(" · req/s %s · failing %s/min", num(m.rate(h, completed)), num(m.perMin(h, failed))), dim})
	out = append(out, slots)
	eng, est := engine(s.metrics)
	out = append(out, line{{"engine ", dim}, {eng, est}})
	if r := s.metrics; r != nil {
		for _, l := range r.Loaded {
			out = append(out, plain("  %s %s, unloads %s", l.Name, size(float64(l.VRAMBytes)), l.ExpiresAt.In(m.Loc).Format("15:04:05")))
		}
		if r.Preemptions > 0 {
			out = append(out, plain("preemptions %s since start", num(r.Preemptions)))
		}
	}
	for _, hi := range s.health {
		if d := time.UnixMilli(hi.CooldownUntilMillis).Sub(m.now()); d > 0 {
			out = append(out, line{{"cooldown ", dim}, {fmt.Sprintf("%s for %s more", hi.Model, dur(d)), bad}})
		}
	}
	if a := s.agent; a != nil {
		out = append(out, plain("agent of key %s from %s · %s · attached %s · %d requests, %d failed",
			a.KeyName, a.Remote, a.State, dur(ms(a.AgeMillis)), a.Requests, a.Failures))
	}
	out = append(out, line{{"models ", dim}, {strings.Join(s.models, ", "), term.Style{}}})
	if h != nil {
		out = append(out, refusalsLine("errors 5m:", h.errors))
	}
	keys := map[string]int{}
	for _, r := range m.St.Requests {
		if r.Backend == name {
			keys[r.KeyName]++
		}
	}
	out = append(out, line{{"live keys ", dim}, {counted(keys), term.Style{}}})
	gs := []graph{{"admitted", m.spark(h, window, gAdmitted), term.Ansi256(114)}}
	if s.metrics != nil && s.metrics.HasQueue {
		gs = append(gs, graph{engineName(s.metrics.Engine) + " waiting", m.spark(h, window, gWaiting), term.Ansi256(214)})
	} else {
		gs = append(gs, graph{"req/s", m.spark(h, window, completed), term.Ansi256(75)})
	}
	return out, gs
}

func (f *frame) modelDetail(name string) ([]line, []graph) {
	m := f.m
	h := m.get("model", name)
	srv := serversOf(m)[name]
	slices.Sort(srv)
	out := []line{
		{{"served by ", dim}, {strings.Join(srv, ", "), term.Style{}}},
		plain("req/s %s · tok/s %s · failing %s/min · refused %s/min", num(m.rate(h, completed)), num(m.rate(h, tokAll)),
			num(m.perMin(h, failed)), num(m.perMin(h, refused))),
	}
	if p50, ok := m.percentile(h, 0.5); ok {
		p95, _ := m.percentile(h, 0.95)
		l := plain("duration p50 %s · p95 %s", dur(ms(p50)), dur(ms(p95)))
		if t, ok := m.ttfb(h); ok {
			l = append(l, seg{fmt.Sprintf(" · first byte %s", dur(t)), term.Style{}})
		}
		out = append(out, l)
	}
	if h != nil {
		out = append(out, refusalsLine("refused 5m:", h.refusals), refusalsLine("errors 5m:", h.errors))
	}
	return out, []graph{{"req/s", m.spark(h, window, completed), term.Ansi256(141)}, {"tok/s", m.spark(h, window, tokAll), term.Ansi256(177)}}
}

func (f *frame) requestDetail(id string) []line {
	for _, r := range f.m.St.Requests {
		if r.RequestID != id {
			continue
		}
		ttfb := "not yet"
		if r.TTFBMillis > 0 {
			ttfb = dur(ms(r.TTFBMillis))
		}
		return []line{
			{{"key ", dim}, {r.KeyName, term.Style{Bold: true}}, {" from ", dim}, {r.Remote, term.Style{}}},
			{{"user agent ", dim}, {r.UserAgent, term.Style{}}},
			plain("%s %s → %s", r.Endpoint, r.Model, r.Backend),
			{{"now ", dim}, {fmt.Sprintf("%s for %s", r.Phase, dur(ms(r.PhaseMillis))), phaseStyle(r.Phase)},
				{fmt.Sprintf(" · age %s · first byte %s", dur(ms(r.AgeMillis)), ttfb), term.Style{}}},
			plain("in %s · out %s · tokens %s", size(float64(r.BytesIn)), size(float64(r.BytesOut)), num(float64(r.Tokens))),
			{{"id ", dim}, {r.RequestID, dim}},
		}
	}
	return []line{plain("finished")}
}

func (f *frame) usageDetail(name string) ([]line, []graph) {
	for _, u := range usageTable(f.m, f.ui.UsageDim) {
		if u.name != name {
			continue
		}
		vals := make([]float64, usagehist.Hours)
		for i, h := range u.hourly {
			vals[i] = float64(h.Charged)
		}
		return nil, []graph{{fmt.Sprintf("tokens charged per hour, last 7 days · %s requests · %s unknown usage",
			num(float64(u.d7.Requests)), num(float64(u.d7.Unknown))), vals, term.Ansi256(141)}}
	}
	return []line{plain("no history")}, nil
}
