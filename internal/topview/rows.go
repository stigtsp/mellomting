package topview

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/agent"
	"mellomting/internal/backendmetrics"
	"mellomting/internal/inflight"
	"mellomting/internal/topview/term"
	"mellomting/internal/usagehist"
)

// row is one listed item. Its cells line up with its view's columns;
// sortv lines up with the view's sort modes.
type row struct {
	id, name string
	kind     string // key, server, model, request, usage
	cells    []cell
	sortv    []float64
}

// cell is one column of a row: text, optionally a meter or a graph.
type cell struct {
	text  string
	st    term.Style
	meter float64   // fraction 0..1 drawn as a bar before text; <0 none
	graph []float64 // drawn as one row of braille when set
	gtop  float64   // the graph's scale; 0 scales to its own peak
	color term.Color
}

func txt(s string) cell                   { return cell{text: s, meter: -1} }
func styled(s string, st term.Style) cell { return cell{text: s, st: st, meter: -1} }

// col is a column: its title, width, alignment, and the order in which
// it is dropped when the terminal is too narrow (0: never).
type col struct {
	title string
	w     int
	right bool
	flex  bool
	drop  int
}

var (
	dim  = term.Style{Dim: true}
	warn = term.Style{FG: term.Ansi256(214)}
	bad  = term.Style{FG: term.Ansi256(203)}
	good = term.Style{FG: term.Ansi256(114)}
)

// columns are each table view's columns.
func columns(v View) []col {
	switch v {
	case Keys:
		return []col{{title: "KEY", w: 14, flex: true}, {title: "INFLIGHT", w: 13}, {title: "REQ/S", w: 9, right: true},
			{title: "REFUSED/MIN", w: 22}, {title: "TOK/S", w: 7, right: true}, {title: "P50", w: 6, right: true, drop: 3},
			{title: "QUOTA", w: 11, drop: 2}, {title: "AT LIMIT", w: 8, right: true, drop: 4}, {title: "LAST 5M", w: 16, drop: 1}}
	case Models:
		return []col{{title: "MODEL", w: 16, flex: true}, {title: "REQ/S", w: 7, right: true}, {title: "TOK/S", w: 7, right: true},
			{title: "ERR/MIN", w: 8, right: true}, {title: "REFUSED/MIN", w: 11, right: true, drop: 3}, {title: "P50", w: 6, right: true, drop: 2},
			{title: "TTFB", w: 6, right: true, drop: 4}, {title: "SERVERS", w: 16, drop: 5}, {title: "LAST 5M", w: 16, drop: 1}}
	case Backends:
		return []col{{title: "SERVER", w: 12, flex: true}, {title: "KIND", w: 6, drop: 4}, {title: "SLOTS", w: 13},
			{title: "QUEUE", w: 7, right: true}, {title: "ENGINE", w: 30}, {title: "REQ/S", w: 7, right: true},
			{title: "ERR/MIN", w: 8, right: true, drop: 3}, {title: "P50", w: 6, right: true, drop: 5}, {title: "LAST 5M", w: 16, drop: 2}}
	case Requests:
		return []col{{title: "KEY", w: 16}, {title: "REMOTE", w: 20, drop: 3}, {title: "MODEL", w: 20}, {title: "SERVER", w: 12, drop: 4},
			{title: "PHASE", w: 9}, {title: "AGE", w: 7, right: true}, {title: "IN PHASE", w: 8, right: true, drop: 5},
			{title: "TTFB", w: 6, right: true, drop: 6}, {title: "OUT", w: 8, right: true, drop: 2}, {title: "USER AGENT", w: 10, flex: true, drop: 1}}
	case Usage:
		return []col{{title: "NAME", w: 14, flex: true}, {title: "REQ 1H", w: 7, right: true, drop: 4}, {title: "TOK 1H", w: 7, right: true},
			{title: "REQ 24H", w: 8, right: true, drop: 3}, {title: "TOK 24H", w: 8, right: true}, {title: "REQ 7D", w: 7, right: true, drop: 2},
			{title: "TOK 7D", w: 7, right: true}, {title: "IN 7D", w: 7, right: true, drop: 5}, {title: "OUT 7D", w: 7, right: true, drop: 5},
			{title: "CACHED", w: 7, right: true, drop: 6}, {title: "UNKNOWN", w: 8, right: true, drop: 1}}
	}
	return nil
}

// viewRows lists the current view's items, filtered and sorted.
func viewRows(m *Model, ui *UIState) []row {
	var rows []row
	switch ui.View {
	case Overview:
		return overviewRows(m, ui)
	case Keys:
		rows = keyRows(m)
	case Models:
		rows = modelRows(m)
	case Backends:
		rows = serverRows(m)
	case Requests:
		rows = requestRows(m)
	case Usage:
		rows = usageRows(m, ui.UsageDim)
	}
	if ui.Filter != "" {
		f := strings.ToLower(ui.Filter)
		rows = slices.DeleteFunc(rows, func(r row) bool {
			return !strings.Contains(strings.ToLower(m.show(ui, r.name)), f)
		})
	}
	if modes := sortModes[ui.View]; len(modes) > 0 {
		i := ui.Sort[ui.View] % len(modes)
		slices.SortStableFunc(rows, func(a, b row) int {
			if modes[i] == "name" || modes[i] == "key" {
				return cmp.Compare(a.name, b.name)
			}
			if c := cmp.Compare(b.sortv[i], a.sortv[i]); c != 0 {
				return c
			}
			return cmp.Compare(a.name, b.name)
		})
	}
	return rows
}

// show is text as displayed: sanitized, and redacted when on.
func (m *Model) show(ui *UIState, s string) string {
	s = term.Sanitize(s)
	if ui.Redact {
		s = m.red.Apply(s)
	}
	return s
}

func (m *Model) keyInfo(id string) *adminapi.KeyInfo {
	for i := range m.St.Keys {
		if m.St.Keys[i].ID == id {
			return &m.St.Keys[i]
		}
	}
	return nil
}

func keyName(m *Model, id string) string {
	if k := m.keyInfo(id); k != nil {
		return k.Name
	}
	if h := m.get("key", id); h != nil && h.name != "" {
		return h.name
	}
	return id
}

// keyRows lists the client keys: every key in the store that is not an
// agent's, and any key series beyond them (the folded "(other)").
func keyRows(m *Model) []row {
	seen := map[string]bool{}
	var rows []row
	add := func(id, name string, k *adminapi.KeyInfo) {
		seen[id] = true
		h := m.get("key", id)
		rps := m.rate(h, completed)
		refMin := m.perMin(h, refused)
		tok := m.rate(h, tokAll)
		r := row{id: id, name: name, kind: "key"}
		inflightN, conc := 0, 0
		var quota cell = styled("–", dim)
		if k != nil {
			inflightN, conc = k.Inflight, k.ConcurrentRequests
			if k.TokensPerHour > 0 {
				f := float64(k.HourUsed) / float64(k.TokensPerHour)
				quota = cell{text: fmt.Sprintf("%3.0f%%", 100*f), meter: f}
			}
		}
		meterCell := styled(fmt.Sprintf("%d", inflightN), term.Style{})
		if conc > 0 {
			f := float64(inflightN) / float64(conc)
			meterCell = cell{text: fmt.Sprintf("%d/%d", inflightN, conc), meter: f}
		}
		rateText := num(rps)
		if k != nil && k.RequestsPerSecond > 0 {
			rateText += "/" + num(k.RequestsPerSecond)
		}
		refCell := styled("0", dim)
		if refMin > 0 {
			reason, _ := dominant(h.refusals)
			refCell = styled(num(refMin)+" "+reason, bad)
		}
		p50 := styled("–", dim)
		if v, ok := m.percentile(h, 0.5); ok {
			p50 = txt(dur(ms(v)))
		}
		lim := styled("–", dim)
		if f, ok := m.atLimit(h); ok && conc > 0 {
			st := term.Style{}
			if f > 0.5 {
				st = warn
			}
			lim = styled(fmt.Sprintf("%.0f%%", 100*f), st)
		}
		r.cells = []cell{txt(name), meterCell, txt(rateText), refCell, txt(num(tok)), p50, quota, lim,
			{graph: m.spark(h, 32, offered), color: term.Ansi256(75), meter: -1}}
		r.sortv = []float64{refMin, float64(inflightN), rps, tok, 0}
		rows = append(rows, r)
	}
	for i := range m.St.Keys {
		k := &m.St.Keys[i]
		if k.Agent != "" {
			continue // an agent's attach key, not a client's
		}
		add(k.ID, k.Name, k)
	}
	for _, id := range m.ids("key") {
		if !seen[id] {
			add(id, keyName(m, id), nil)
		}
	}
	return rows
}

// serversOf maps each model to the servers and agents serving it.
func serversOf(m *Model) map[string][]string {
	out := map[string][]string{}
	for _, s := range m.St.Servers {
		for _, model := range s.Models {
			out[model] = append(out[model], s.Name)
		}
	}
	for _, a := range m.St.Agents {
		for _, model := range a.Models {
			out[model.Name] = append(out[model.Name], a.Name)
		}
	}
	return out
}

func modelRows(m *Model) []row {
	where := serversOf(m)
	names := map[string]bool{}
	for model := range where {
		names[model] = true
	}
	for _, id := range m.ids("model") {
		names[id] = true
	}
	var rows []row
	for _, name := range slices.Sorted(mapsKeys(names)) {
		h := m.get("model", name)
		rps, tok := m.rate(h, completed), m.rate(h, tokAll)
		errMin, refMin := m.perMin(h, failed), m.perMin(h, refused)
		p50, ttfb := styled("–", dim), styled("–", dim)
		if v, ok := m.percentile(h, 0.5); ok {
			p50 = txt(dur(ms(v)))
		}
		if d, ok := m.ttfb(h); ok {
			ttfb = txt(dur(d))
		}
		errCell := styled("0", dim)
		if errMin > 0 {
			errCell = styled(num(errMin), bad)
		}
		refCell := styled("0", dim)
		if refMin > 0 {
			refCell = styled(num(refMin), bad)
		}
		srv := where[name]
		slices.Sort(srv)
		rows = append(rows, row{id: name, name: name, kind: "model",
			cells: []cell{txt(name), txt(num(rps)), txt(num(tok)), errCell, refCell, p50, ttfb, txt(strings.Join(srv, ", ")),
				{graph: m.spark(h, 32, completed), color: term.Ansi256(141), meter: -1}},
			sortv: []float64{rps, tok, errMin, 0}})
	}
	return rows
}

func mapsKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// server is one row of the Backends view: a configured server or an
// agent attachment.
type server struct {
	name, kind       string
	admitted, max    int
	queued, queueMax int
	metrics          *backendmetrics.Reading
	health           []adminapi.HealthInfo
	models           []string
	agent            *agent.Record
}

func servers(m *Model) []server {
	var out []server
	for _, s := range m.St.Servers {
		out = append(out, server{name: s.Name, kind: "cfg", admitted: s.Admitted, max: s.MaxConcurrency, queued: s.Queued,
			queueMax: s.QueueSize, metrics: s.Metrics, health: s.Health, models: s.Models})
	}
	for i := range m.St.Agents {
		a := &m.St.Agents[i]
		var models []string
		for _, mo := range a.Models {
			models = append(models, mo.Name)
		}
		out = append(out, server{name: a.Name, kind: "agent", admitted: a.Admitted, max: a.MaxConcurrency, queued: a.Queued,
			metrics: a.Metrics, models: models, agent: a})
	}
	return out
}

func (m *Model) server(name string) *server {
	for _, s := range servers(m) {
		if s.name == name {
			return &s
		}
	}
	return nil
}

// engine describes a server's own report: queue and cache for vLLM and
// SGLang, loaded models for Ollama.
func engine(r *backendmetrics.Reading) (string, term.Style) {
	if r == nil {
		return "no metrics", dim
	}
	if !r.HasQueue {
		var vram int64
		for _, l := range r.Loaded {
			vram += l.VRAMBytes
		}
		return fmt.Sprintf("%s %d loaded %s", engineName(r.Engine), len(r.Loaded), size(float64(vram))), term.Style{}
	}
	s := fmt.Sprintf("%s %.0f run %.0f wait KV %.0f%%", engineName(r.Engine), r.Running, r.Waiting, r.KVPercent)
	switch {
	case r.Waiting > 0:
		return s, warn
	case r.KVPercent >= 90:
		return s, warn
	}
	return s, term.Style{}
}

func cooldown(s server, now time.Time) (time.Duration, string) {
	var longest time.Duration
	var model string
	for _, h := range s.health {
		if d := time.UnixMilli(h.CooldownUntilMillis).Sub(now); d > longest {
			longest, model = d, h.Model
		}
	}
	return longest, model
}

func serverRows(m *Model) []row {
	var rows []row
	now := m.now()
	for _, s := range servers(m) {
		h := m.get("server", s.name)
		rps, errMin := m.rate(h, completed), m.perMin(h, failed)
		slots := txt(fmt.Sprintf("%d", s.admitted))
		load := 0.0
		if s.max > 0 {
			load = float64(s.admitted) / float64(s.max)
			slots = cell{text: fmt.Sprintf("%d/%d", s.admitted, s.max), meter: load}
		}
		queue := styled("–", dim)
		if s.queueMax > 0 || s.queued > 0 {
			st := dim
			if s.queued > 0 {
				st = warn
			}
			queue = styled(fmt.Sprintf("%d/%d", s.queued, s.queueMax), st)
		}
		eng, est := engine(s.metrics)
		if d, model := cooldown(s, now); d > 0 {
			eng, est = fmt.Sprintf("cooldown %s %s", dur(d), model), bad
		}
		errCell := styled("0", dim)
		if errMin > 0 {
			errCell = styled(num(errMin), bad)
		}
		p50 := styled("–", dim)
		if v, ok := m.percentile(h, 0.5); ok {
			p50 = txt(dur(ms(v)))
		}
		kind := s.kind
		rows = append(rows, row{id: s.name, name: s.name, kind: "server",
			cells: []cell{txt(s.name), styled(kind, dim), slots, queue, styled(eng, est), txt(num(rps)), errCell, p50,
				{graph: m.spark(h, 32, completed), color: term.Ansi256(114), meter: -1}},
			sortv: []float64{load + float64(s.queued), rps, errMin, 0}})
	}
	return rows
}

var phaseOrder = map[inflight.Phase]float64{inflight.PhaseReading: 1, inflight.PhaseRouting: 2, inflight.PhaseQueued: 6,
	inflight.PhaseWaiting: 5, inflight.PhaseStreaming: 4, inflight.PhaseSending: 3}

func phaseStyle(p inflight.Phase) term.Style {
	switch p {
	case inflight.PhaseQueued:
		return bad
	case inflight.PhaseWaiting:
		return warn
	case inflight.PhaseStreaming, inflight.PhaseSending:
		return good
	}
	return dim
}

func requestRows(m *Model) []row {
	var rows []row
	for _, r := range m.St.Requests {
		ttfb := styled("–", dim)
		if r.TTFBMillis > 0 {
			ttfb = txt(dur(ms(r.TTFBMillis)))
		}
		rows = append(rows, row{id: r.RequestID, name: r.KeyName, kind: "request",
			cells: []cell{txt(r.KeyName), txt(r.Remote), txt(r.Model), txt(r.Backend), styled(string(r.Phase), phaseStyle(r.Phase)),
				txt(dur(ms(r.AgeMillis))), txt(dur(ms(r.PhaseMillis))), ttfb, txt(size(float64(r.BytesOut))), styled(r.UserAgent, dim)},
			sortv: []float64{float64(r.AgeMillis), phaseOrder[r.Phase], 0, float64(r.BytesOut)}})
	}
	return rows
}

// usageRow sums one name's history rows (several backends may map to
// one server) over three windows.
type usageRow struct {
	name        string
	h1, h24, d7 usagehist.Row
	hourly      [usagehist.Hours]usagehist.Row
}

func (m *Model) usageName(which int, id string) string {
	switch which {
	case 0:
		return keyName(m, id)
	case 2:
		if s, ok := m.backendServer[id]; ok {
			return s
		}
	}
	return id
}

func usageTable(m *Model, which int) []usageRow {
	if m.usage == nil {
		return nil
	}
	now := m.now()
	d := usagehist.Dim(which)
	byName := map[string]*usageRow{}
	var order []string
	get := func(id string) *usageRow {
		name := m.usageName(which, id)
		u := byName[name]
		if u == nil {
			u = &usageRow{name: name}
			byName[name] = u
			order = append(order, name)
		}
		return u
	}
	add := func(dst *usagehist.Row, r usagehist.Row) {
		dst.Requests += r.Requests
		dst.In += r.In
		dst.Out += r.Out
		dst.Cached += r.Cached
		dst.Charged += r.Charged
		dst.Unknown += r.Unknown
	}
	for _, r := range m.usage.Totals(d, usagehist.Span, now) {
		u := get(r.ID)
		add(&u.d7, r)
		hours := m.usage.Hourly(d, r.ID, now)
		for i := range hours {
			add(&u.hourly[i], hours[i])
		}
	}
	for _, r := range m.usage.Totals(d, 24*time.Hour, now) {
		add(&get(r.ID).h24, r)
	}
	for _, r := range m.usage.Totals(d, time.Hour, now) {
		add(&get(r.ID).h1, r)
	}
	out := make([]usageRow, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	slices.SortFunc(out, func(a, b usageRow) int {
		if c := cmp.Compare(b.d7.Charged, a.d7.Charged); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	return out
}

func usageRows(m *Model, which int) []row {
	var rows []row
	for _, u := range usageTable(m, which) {
		unknown := styled("0", dim)
		if u.d7.Unknown > 0 {
			unknown = styled(num(float64(u.d7.Unknown)), warn)
		}
		rows = append(rows, row{id: u.name, name: u.name, kind: "usage",
			cells: []cell{txt(u.name), txt(num(float64(u.h1.Requests))), txt(num(float64(u.h1.Charged))),
				txt(num(float64(u.h24.Requests))), txt(num(float64(u.h24.Charged))), txt(num(float64(u.d7.Requests))),
				styled(num(float64(u.d7.Charged)), term.Style{Bold: true}), txt(num(float64(u.d7.In))), txt(num(float64(u.d7.Out))),
				txt(num(float64(u.d7.Cached))), unknown}})
	}
	return rows
}

// overviewRows are the dashboard's selectable items: the busiest keys,
// servers and models, in that order.
func overviewRows(m *Model, ui *UIState) []row {
	n := topN(ui.Height)
	pick := func(rows []row, by int) []row {
		slices.SortStableFunc(rows, func(a, b row) int {
			if c := cmp.Compare(b.sortv[by], a.sortv[by]); c != 0 {
				return c
			}
			return cmp.Compare(a.name, b.name)
		})
		return rows[:min(n, len(rows))]
	}
	var out []row
	for _, r := range pick(keyRows(m), 2) {
		r.id = "key:" + r.id
		out = append(out, r)
	}
	for _, r := range pick(serverRows(m), 0) {
		r.id = "server:" + r.id
		out = append(out, r)
	}
	for _, r := range pick(modelRows(m), 0) {
		r.id = "model:" + r.id
		out = append(out, r)
	}
	return out
}

func topN(h int) int {
	if h >= 34 {
		return 5
	}
	return 3
}
