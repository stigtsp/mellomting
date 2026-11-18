package topview

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"mellomting/internal/topview/term"
)

// attn is one thing holding traffic back or going wrong. score orders
// them: losing accounting records first, then a server out of rotation;
// below those, a request queued weighs as two refused or failed per
// minute: a queue is a whole server's backlog, a refusal one client.
type attn struct {
	score float64
	text  string
	st    term.Style
	id    string // the item to select for its detail, if any
}

// attention lists what an operator should look at, worst first.
func attention(m *Model) []attn {
	var out []attn
	add := func(score float64, st term.Style, id, format string, args ...any) {
		out = append(out, attn{score: score, text: fmt.Sprintf(format, args...), st: st, id: id})
	}
	if d := m.St.Global.Accounting.Dropped; d > 0 {
		add(1e7, bad, "", "accounting dropped %d records: the disk is not keeping up", d)
	}
	now := m.now()
	for _, s := range servers(m) {
		if d, model := cooldown(s, now); d > 0 {
			add(1e6+d.Seconds(), bad, "server:"+s.name, "%s in cooldown %s for %s", s.name, dur(d), model)
		}
		if s.queued > 0 {
			add(2*float64(s.queued), warn, "server:"+s.name, "%s edge queue %d/%d: max_concurrency %d reached", s.name, s.queued, s.queueMax, s.max)
		}
		if r := s.metrics; r != nil && r.HasQueue && r.Waiting > 0 {
			add(2*r.Waiting, warn, "server:"+s.name, "%s %s queueing %.0f (%.0f running, KV %.0f%%)",
				s.name, engineName(r.Engine), r.Waiting, r.Running, r.KVPercent)
		}
		h := m.get("server", s.name)
		if n := m.perMin(h, failed); n > 0 {
			class, _ := dominant(h.errors)
			add(n, bad, "server:"+s.name, "%s failing %s/min (%s)", s.name, num(n), class)
		}
	}
	for _, k := range keyRows(m) {
		h := m.get("key", k.id)
		n := m.perMin(h, refused)
		if n <= 0 {
			continue
		}
		reason, _ := dominant(h.refusals)
		limit := limitName(reason)
		if ki := m.keyInfo(k.id); ki != nil {
			switch reason {
			case "key_concurrency":
				limit = fmt.Sprintf("%s (%d)", limit, ki.ConcurrentRequests)
			case "key_rate":
				limit = fmt.Sprintf("%s (%s)", limit, num(ki.RequestsPerSecond))
			}
		}
		add(n, bad, "key:"+k.id, "%s refused %s/min by %s", k.name, num(n), limit)
	}
	// Refusals no key is to blame for: the daemon-wide limits and
	// unauthenticated requests.
	if counts := m.globalRefusals(60); len(counts) > 0 {
		for reason, n := range counts {
			switch reason {
			case "global_inflight", "global_rate", "source_rate":
				add(n, bad, "", "%s refused %s/min", limitName(reason), num(n))
			case "auth":
				add(n/10, dim, "", "%s/min refused with a bad or missing key", num(n))
			}
		}
	}
	for i := range m.St.Keys {
		k := &m.St.Keys[i]
		if k.TokensPerHour > 0 && float64(k.HourUsed) >= 0.9*float64(k.TokensPerHour) {
			add(50, warn, "key:"+k.ID, "%s at %.0f%% of tokens_per_hour", k.Name, 100*float64(k.HourUsed)/float64(k.TokensPerHour))
		}
	}
	slices.SortStableFunc(out, func(a, b attn) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return strings.Compare(a.text, b.text)
	})
	if len(out) == 0 {
		return []attn{{text: "nothing held back", st: good}}
	}
	return out
}

// globalRefusals counts refusals by reason over the last n seconds.
func (m *Model) globalRefusals(n int) map[string]float64 {
	order := m.St.Stats.RefusalOrder
	out := map[string]float64{}
	for i := range n {
		b := m.global.at(m.seq - int64(i))
		if b == nil || b.Refusals == nil {
			continue
		}
		for j, c := range b.Refusals {
			if c > 0 && j < len(order) {
				out[string(order[j])] += float64(c)
			}
		}
	}
	return out
}
