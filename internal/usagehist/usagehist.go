// Package usagehist reads the accounting log, current and rotated, into
// hourly totals for `mellomting top`'s usage view (PLAN §43.2). It reads
// the file the daemon writes and nothing else: top stays a viewer, and
// the daemon serves no history.
//
// Rotated files follow deploy/mellomting.logrotate: usage.jsonl.1 plain
// (delaycompress), usage.jsonl.2.gz and older compressed. Files are read
// newest first, so when the byte budget runs out it is the oldest hours
// that are missing, and Covers says where the cut fell.
package usagehist

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"
)

const (
	// Span is how far back history is kept.
	Span = 7 * 24 * time.Hour
	// Hours is the number of hourly buckets Hourly returns.
	Hours = int(Span / time.Hour)
	// MaxBytes bounds how much log one Open reads, decompressed.
	MaxBytes = 512 << 20
	// maxLine skips a line longer than any record the daemon writes.
	maxLine = 16 << 10
	// maxFiles bounds the rotated files looked for.
	maxFiles = 64
	// maxIDs bounds the distinct names per dimension; the rest fold
	// into OtherID. Model names come from agents.
	maxIDs = 256
	// OtherID collects names past maxIDs.
	OtherID = "(other)"
)

// Dim is what totals are grouped by.
type Dim int

// Dimensions.
const (
	ByKey Dim = iota
	ByModel
	ByServer
)

// Row is the totals of one key, model or server.
type Row struct {
	ID                                          string
	Requests, In, Out, Cached, Charged, Unknown int64
}

func (r *Row) add(o Row) {
	r.Requests += o.Requests
	r.In += o.In
	r.Out += o.Out
	r.Cached += o.Cached
	r.Charged += o.Charged
	r.Unknown += o.Unknown
}

// History is the hourly totals read so far. It is not safe for
// concurrent use; top refreshes and renders from one goroutine.
type History struct {
	path      string
	budget    int
	dims      [3]map[string]map[int64]*Row // id → unix hour → totals
	from, to  time.Time
	truncated bool

	// The current file's read position, and its identity to notice
	// rotation by rename.
	offset int64
	info   os.FileInfo
}

// Open reads the accounting log at path and its rotations.
func Open(path string, now time.Time) (*History, error) { return open(path, now, MaxBytes) }

func open(path string, now time.Time, budget int) (*History, error) {
	h := &History{path: path, budget: budget}
	return h, h.load(now)
}

func (h *History) load(now time.Time) error {
	for i := range h.dims {
		h.dims[i] = make(map[string]map[int64]*Row)
	}
	h.from, h.to, h.truncated, h.offset, h.info = time.Time{}, time.Time{}, false, 0, nil

	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if h.info, err = f.Stat(); err != nil {
		return err
	}
	bound := now.Add(-Span)
	left := h.budget
	// A live file larger than the budget is read from its tail, so what
	// the cut loses is its oldest records.
	var start int64
	if size := h.info.Size(); size > int64(left) {
		start = size - int64(left)
		if start, err = skipToLine(f, start); err != nil {
			return err
		}
		h.truncated = true
	}
	n, _, _, err := h.scan(f, bound, -1, true)
	if err != nil {
		return err
	}
	h.offset = start + n
	left -= int(n)
	cut := h.truncated
	for i := 1; i <= maxFiles && !cut && left > 0; i++ {
		rf, gz, err := openRotated(h.path, i)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		var r io.Reader = rf
		if gz {
			zr, err := gzip.NewReader(rf)
			if err != nil {
				rf.Close()
				break // a corrupt archive ends the history
			}
			r = zr
		}
		var read int64
		var newest time.Time
		read, newest, cut, err = h.scan(r, bound, left, false)
		rf.Close()
		if err != nil {
			break
		}
		left -= int(read)
		// A file wholly older than the span: the older ones are too.
		if newest.Before(bound) {
			break
		}
	}
	if cut || left <= 0 {
		h.truncated = true
	}
	return nil
}

// skipToLine positions f at the first line that starts at or after off
// and returns that offset.
func skipToLine(f *os.File, off int64) (int64, error) {
	if _, err := f.Seek(off-1, io.SeekStart); err != nil {
		return 0, err
	}
	br := bufio.NewReader(f)
	skipped, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	pos := off - 1 + int64(len(skipped))
	_, err = f.Seek(pos, io.SeekStart)
	return pos, err
}

func openRotated(path string, i int) (*os.File, bool, error) {
	f, err := os.Open(fmt.Sprintf("%s.%d", path, i))
	if err == nil {
		return f, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	f, err = os.Open(fmt.Sprintf("%s.%d.gz", path, i))
	return f, true, err
}

// scan folds complete lines from r into the totals, reading at most
// budget bytes. It returns the bytes consumed (up to the last complete
// line, so a tail picks up a half-written record when it is finished),
// the newest record time seen, and whether the budget cut it short.
func (h *History) scan(r io.Reader, bound time.Time, budget int, tail bool) (int64, time.Time, bool, error) {
	br := bufio.NewReaderSize(r, maxLine)
	var consumed int64
	var newest time.Time
	for {
		if budget >= 0 && consumed >= int64(budget) {
			return consumed, newest, true, nil
		}
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			skipped := int64(len(line))
			for errors.Is(err, bufio.ErrBufferFull) {
				if budget >= 0 && consumed+skipped >= int64(budget) {
					return consumed + skipped, newest, true, nil
				}
				line, err = br.ReadSlice('\n')
				skipped += int64(len(line))
			}
			if err != nil {
				// An over-long line still being written: leave it.
				return consumed, newest, false, ignoreEOF(err)
			}
			consumed += skipped
			continue
		}
		if err != nil {
			// A last line without its newline is unfinished in the live
			// file; in a rotated one it is complete.
			if !tail && len(line) > 0 && errors.Is(err, io.EOF) {
				consumed += int64(len(line))
				if t, ok := h.fold(line, bound); ok && t.After(newest) {
					newest = t
				}
			}
			return consumed, newest, false, ignoreEOF(err)
		}
		consumed += int64(len(line))
		if t, ok := h.fold(line, bound); ok && t.After(newest) {
			newest = t
		}
	}
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type record struct {
	Time    time.Time `json:"time"`
	KeyID   string    `json:"key_id"`
	Model   string    `json:"model"`
	Backend string    `json:"backend"`
	In      int64     `json:"input_tokens"`
	Out     int64     `json:"output_tokens"`
	Cached  int64     `json:"cached_tokens"`
	Charged int64     `json:"charged_tokens"`
	Status  string    `json:"usage_status"`
}

// fold adds one record. It reports the record's time, also for one
// older than bound, which is not added.
func (h *History) fold(line []byte, bound time.Time) (time.Time, bool) {
	var rec record
	if json.Unmarshal(line, &rec) != nil || rec.Time.IsZero() {
		return time.Time{}, false
	}
	if rec.Time.Before(bound) {
		return rec.Time, true
	}
	row := Row{Requests: 1, In: max(rec.In, 0), Out: max(rec.Out, 0), Cached: max(rec.Cached, 0), Charged: max(rec.Charged, 0)}
	if rec.Status == "unknown" {
		row.Unknown = 1
	}
	hour := rec.Time.Unix() / 3600
	for d, id := range [3]string{rec.KeyID, rec.Model, server(rec.Backend)} {
		if id == "" {
			continue
		}
		m := h.dims[d]
		hours, ok := m[id]
		if !ok {
			if len(m) >= maxIDs {
				id = OtherID
				hours = m[id]
			}
			if hours == nil {
				hours = make(map[int64]*Row)
				m[id] = hours
			}
		}
		b := hours[hour]
		if b == nil {
			b = &Row{}
			hours[hour] = b
		}
		b.add(row)
	}
	if h.from.IsZero() || rec.Time.Before(h.from) {
		h.from = rec.Time
	}
	if rec.Time.After(h.to) {
		h.to = rec.Time
	}
	return rec.Time, true
}

var attachment = regexp.MustCompile(`^(agent-.+)-[0-9]+$`)

// server folds an agent attachment's backend name, which carries a
// sequence number that changes on every reconnect, into one name per
// agent key.
func server(backend string) string {
	if m := attachment.FindStringSubmatch(backend); m != nil {
		return m[1]
	}
	return backend
}

// Refresh reads what was appended since the last read. When the file
// was truncated (copytruncate) or replaced (rename rotation), it reads
// everything again, so a rotated record is counted once.
func (h *History) Refresh(now time.Time) error {
	info, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	if h.info == nil || !os.SameFile(info, h.info) || info.Size() < h.offset {
		return h.load(now)
	}
	if info.Size() == h.offset {
		h.prune(now)
		return nil
	}
	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(h.offset, io.SeekStart); err != nil {
		return err
	}
	n, _, _, err := h.scan(f, now.Add(-Span), -1, true)
	h.offset += n
	h.info = info
	h.prune(now)
	return err
}

// prune drops hours that left the span.
func (h *History) prune(now time.Time) {
	first := now.Add(-Span).Unix() / 3600
	for _, m := range h.dims {
		for id, hours := range m {
			for hr := range hours {
				if hr < first {
					delete(hours, hr)
				}
			}
			if len(hours) == 0 {
				delete(m, id)
			}
		}
	}
}

// Totals sums each name's hours within window of now, largest charge
// first.
func (h *History) Totals(dim Dim, window time.Duration, now time.Time) []Row {
	first := now.Add(-window).Unix() / 3600
	var out []Row
	for id, hours := range h.dims[dim] {
		r := Row{ID: id}
		for hr, b := range hours {
			if hr >= first {
				r.add(*b)
			}
		}
		if r.Requests > 0 {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Row) int {
		if c := cmp.Compare(b.Charged, a.Charged); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// Hourly returns one name's last Hours hours, oldest first; the last
// entry is the current hour.
func (h *History) Hourly(dim Dim, id string, now time.Time) [Hours]Row {
	var out [Hours]Row
	cur := now.Unix() / 3600
	for hr, b := range h.dims[dim][id] {
		if i := Hours - 1 - int(cur-hr); i >= 0 && i < Hours {
			out[i] = *b
			out[i].ID = id
		}
	}
	return out
}

// Covers is the time of the oldest and newest record read.
func (h *History) Covers() (from, to time.Time) { return h.from, h.to }

// Truncated reports whether the byte budget stopped the read before the
// span was covered.
func (h *History) Truncated() bool { return h.truncated }
