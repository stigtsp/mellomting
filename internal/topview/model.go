// Package topview is `mellomting top`: an interactive viewer of the
// daemon's admin data (PLAN §43.2). It only reads: nothing here changes
// the daemon. Overview, the dashboard it opens on, answers "why is this
// slow or held back, and where?"; the other views list keys, models,
// servers, live requests and accounting history, each with a detail box.
package topview

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/stats"
	"mellomting/internal/usagehist"
)

const window = int(stats.Window / time.Second)

// hist is one series as received: a ring of per-second buckets and the
// daemon's totals over its window.
type hist struct {
	name     string
	ring     [window]stats.BucketExport
	requests int64
	errors   map[string]int64
	refusals map[string]int64
}

func (h *hist) merge(se stats.SeriesExport) {
	if se.Name != "" {
		h.name = se.Name
	}
	h.requests, h.errors, h.refusals = se.Requests, se.Errors, se.Refusals
	// Null buckets mean the reply was truncated: keep what is here.
	for _, b := range se.Buckets {
		if b.Seq > 0 {
			h.ring[b.Seq%int64(window)] = b
		}
	}
}

func (h *hist) at(seq int64) *stats.BucketExport {
	if h == nil || seq <= 0 {
		return nil
	}
	if b := &h.ring[seq%int64(window)]; b.Seq == seq {
		return b
	}
	return nil
}

// values is n seconds of f ending at end, oldest first; seconds without
// a bucket are zero.
func (h *hist) values(end int64, n int, f func(*stats.BucketExport) float64) []float64 {
	out := make([]float64, n)
	for i := range n {
		if b := h.at(end - int64(n-1-i)); b != nil {
			out[i] = f(b)
		}
	}
	return out
}

// mean is f averaged over the n seconds ending at end.
func (h *hist) mean(end int64, n int, f func(*stats.BucketExport) float64) float64 {
	var sum float64
	for _, v := range h.values(end, n, f) {
		sum += v
	}
	return sum / float64(n)
}

func (h *hist) sum(end int64, n int, f func(*stats.BucketExport) float64) float64 {
	return h.mean(end, n, f) * float64(n)
}

type sid struct{ dim, id string }

// Model is everything received from the daemon, merged across polls.
type Model struct {
	Loc *time.Location
	// St is the latest reply; its series have been merged into the
	// history below.
	St      adminapi.Stats
	have    bool
	seq     int64
	started int64
	global  hist
	series  map[sid]*hist
	edges   []int64

	err string // why the last poll failed; the banner

	red *Redactor

	usage         *usagehist.History
	usageErr      string
	backendServer map[string]string
}

// NewModel is an empty model; loc is the time zone of displayed clocks.
func NewModel(loc *time.Location) *Model {
	if loc == nil {
		loc = time.Local
	}
	return &Model{Loc: loc, series: map[sid]*hist{}, red: NewRedactor()}
}

// SetUsage attaches the accounting history, or why there is none.
// backendServer maps a configured backend's generated name to the
// server it runs on, so the Usage view lists servers, not
// auto-<server>-<hex> names.
func (m *Model) SetUsage(h *usagehist.History, why string, backendServer map[string]string) {
	m.usage, m.usageErr, m.backendServer = h, why, backendServer
}

// Since is the seq to ask for next: 0 for the whole window.
func (m *Model) Since() int64 {
	if !m.have {
		return 0
	}
	return m.seq
}

// Apply merges a reply. It returns true, and keeps nothing, when the
// daemon restarted or its clock went back: the reply is a delta against
// history that no longer exists, so the caller asks again with since=0.
func (m *Model) Apply(st adminapi.Stats) (reset bool) {
	m.err = ""
	if m.have && (st.StartedMillis != m.started || st.Stats.Seq < m.seq) {
		m.reset()
		return true
	}
	m.St, m.have, m.started, m.seq = st, true, st.StartedMillis, st.Stats.Seq
	m.global.merge(st.Stats.Global)
	seen := make(map[sid]bool, len(st.Stats.Series))
	for _, se := range st.Stats.Series {
		k := sid{se.Dim, se.ID}
		h := m.series[k]
		if h == nil {
			h = &hist{}
			m.series[k] = h
		}
		h.merge(se)
		seen[k] = true
	}
	// A series the daemon no longer sends was idle for its whole window.
	for k := range m.series {
		if !seen[k] {
			delete(m.series, k)
		}
	}
	if len(st.Stats.HistEdgesMs) > 0 {
		m.edges = st.Stats.HistEdgesMs
	}
	return false
}

func (m *Model) reset() {
	m.St, m.have, m.seq, m.started = adminapi.Stats{}, false, 0, 0
	m.global = hist{}
	m.series = map[sid]*hist{}
}

// Fail records a failed poll. The data already shown stays.
func (m *Model) Fail(err error) { m.err = describe(err) }

// Banner is an error a Source can return to say what the banner should
// read, such as a daemon too old for this view.
type Banner interface{ Banner() string }

// describe says why the daemon could not be read, without the socket
// path or other detail from the error text.
func describe(err error) string {
	var b Banner
	switch msg := err.Error(); {
	case errors.As(err, &b):
		return b.Banner()
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout"):
		return "the daemon did not answer in time"
	case strings.Contains(msg, "connection refused"):
		return "connection refused: is the daemon running?"
	case strings.Contains(msg, "no such file"):
		return "the admin socket is missing: is the daemon running?"
	case strings.Contains(msg, "permission denied"):
		return "permission denied on the admin socket"
	}
	return "cannot read the daemon"
}

// now is the daemon's clock at the latest reply.
func (m *Model) now() time.Time {
	if m.St.NowMillis == 0 {
		return time.Now().In(m.Loc)
	}
	return time.UnixMilli(m.St.NowMillis).In(m.Loc)
}

func (m *Model) get(dim, id string) *hist { return m.series[sid{dim, id}] }

// ids lists one dimension's series IDs, sorted.
func (m *Model) ids(dim string) []string {
	var out []string
	for k := range m.series {
		if k.dim == dim {
			out = append(out, k.id)
		}
	}
	slices.Sort(out)
	return out
}

// Per-bucket figures.
func completed(b *stats.BucketExport) float64 { return float64(b.OK + b.C4xx + b.C5xx) }
func offered(b *stats.BucketExport) float64   { return float64(b.OK + b.C4xx + b.C5xx + b.Refused) }
func refused(b *stats.BucketExport) float64   { return float64(b.Refused) }
func failed(b *stats.BucketExport) float64    { return float64(b.C5xx) }
func tokOut(b *stats.BucketExport) float64    { return float64(b.TokensOut) }
func tokIn(b *stats.BucketExport) float64     { return float64(b.TokensIn) }
func tokAll(b *stats.BucketExport) float64    { return float64(b.TokensIn + b.TokensOut) }
func bytesIn(b *stats.BucketExport) float64   { return float64(b.BytesIn) }
func bytesOut(b *stats.BucketExport) float64  { return float64(b.BytesOut) }
func gAdmitted(b *stats.BucketExport) float64 { return float64(b.Admitted) }
func gWaiting(b *stats.BucketExport) float64  { return float64(b.Waiting) }

// rate is a per-second figure over the last ten complete seconds:
// steadier than one second, still current.
func (m *Model) rate(h *hist, f func(*stats.BucketExport) float64) float64 {
	if h == nil {
		return 0
	}
	return h.mean(m.seq, 10, f)
}

// perMin is a count over the last minute.
func (m *Model) perMin(h *hist, f func(*stats.BucketExport) float64) float64 {
	if h == nil {
		return 0
	}
	return h.sum(m.seq, 60, f)
}

// spark is the whole window of f, n points, each the mean of its
// stretch of seconds.
func (m *Model) spark(h *hist, n int, f func(*stats.BucketExport) float64) []float64 {
	if h == nil || n <= 0 {
		return make([]float64, max(n, 0))
	}
	return downsample(h.values(m.seq, window, f), n)
}

func downsample(v []float64, n int) []float64 {
	if n >= len(v) {
		return v
	}
	out := make([]float64, n)
	for i := range n {
		lo, hi := i*len(v)/n, (i+1)*len(v)/n
		var s float64
		for _, x := range v[lo:hi] {
			s += x
		}
		out[i] = s / float64(max(hi-lo, 1))
	}
	return out
}

// percentile estimates a duration percentile over the window from the
// histogram, interpolating within the bucket it falls in; ok is false
// with no completed requests.
func (m *Model) percentile(h *hist, p float64) (ms int64, ok bool) {
	if h == nil || len(m.edges) == 0 {
		return 0, false
	}
	counts := make([]float64, len(m.edges)+1)
	var total float64
	for i := range window {
		b := &h.ring[i]
		if b.Hist == nil || b.Seq <= m.seq-int64(window) || b.Seq > m.seq {
			continue
		}
		for j, c := range b.Hist {
			if j < len(counts) {
				counts[j] += float64(c)
				total += float64(c)
			}
		}
	}
	if total == 0 {
		return 0, false
	}
	var run float64
	for j, c := range counts {
		if c > 0 && run+c >= p*total {
			if j >= len(m.edges) {
				return m.edges[len(m.edges)-1], true // past the last edge
			}
			lo := int64(0)
			if j > 0 {
				lo = m.edges[j-1]
			}
			frac := (p*total - run) / c
			return lo + int64(frac*float64(m.edges[j]-lo)), true
		}
		run += c
	}
	return m.edges[len(m.edges)-1], true
}

// ttfb is the mean time to first byte over the window.
func (m *Model) ttfb(h *hist) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	var sum, n int64
	for i := range window {
		b := &h.ring[i]
		if b.Seq > m.seq-int64(window) && b.Seq <= m.seq {
			sum += b.TTFBSumMs
			n += int64(b.TTFBN)
		}
	}
	if n == 0 {
		return 0, false
	}
	return time.Duration(sum/n) * time.Millisecond, true
}

// atLimit is the share of gauged seconds a key spent at its
// concurrency bound.
func (m *Model) atLimit(h *hist) (float64, bool) {
	if h == nil {
		return 0, false
	}
	var gauged, at int
	for i := range window {
		b := &h.ring[i]
		if b.Seq > m.seq-int64(window) && b.Seq <= m.seq && b.Gauged {
			gauged++
			if b.AtLimit {
				at++
			}
		}
	}
	if gauged == 0 {
		return 0, false
	}
	return float64(at) / float64(gauged), true
}

// recentRefusal is the reason behind most refusals in the last n
// seconds, from the global series, which keeps every reason per
// second.
func (m *Model) recentRefusal(n int) (string, float64) {
	order := m.St.Stats.RefusalOrder
	counts := make([]float64, len(order))
	for i := range n {
		if b := m.global.at(m.seq - int64(i)); b != nil && b.Refusals != nil {
			for j, c := range b.Refusals {
				if j < len(counts) {
					counts[j] += float64(c)
				}
			}
		}
	}
	best := -1
	for j, c := range counts {
		if c > 0 && (best < 0 || c > counts[best]) {
			best = j
		}
	}
	if best < 0 {
		return "", 0
	}
	return string(order[best]), counts[best] / float64(n)
}

// dominant is the largest entry of a totals map.
func dominant(m map[string]int64) (string, int64) {
	var best string
	var n int64
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best, n
}
