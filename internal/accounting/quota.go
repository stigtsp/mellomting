package accounting

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"os"
	"sync"
	"time"
)

// Quota holds per-key fixed UTC token windows (PLAN §39-40). State is
// updated synchronously at settlement, independently of the JSONL writer,
// so an accounting-disk stall never disables live quota enforcement
// (PLAN §42).
type Quota struct {
	mu     sync.Mutex
	states map[string]*keyQuota
	clock  func() time.Time
}

// keyQuota is one key's current-hour and current-day token totals.
type keyQuota struct {
	hourKey int64 // UTC hour window start (unix)
	hour    int64 // tokens settled in the current hour
	dayKey  int64 // UTC day window start (unix)
	day     int64 // tokens settled in the current day
}

// NewQuota builds an empty quota tracker using the real clock.
func NewQuota() *Quota { return &Quota{} }

// WindowLimit exposes the token window limits configured on a key.
type WindowLimit struct {
	TokensPerHour int64
	TokensPerDay  int64
}

// satAdd adds two non-negative token counts without wrapping: the result
// saturates at MaxInt64 so overflow can never make a quota counter
// negative or defeat a comparison (PLAN §39).
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Admit checks the settled usage against the key's configured windows,
// optionally reserving extra output capacity. It returns false plus a
// machine-readable reason ("tokens_per_hour" or "tokens_per_day") when
// the key is clearly over quota (PLAN §39). A limit of 0 means unlimited.
// The reservation comparison is written to avoid overflow: settled +
// reservation > limit is checked as reservation > limit || settled >
// limit - reservation.
func (q *Quota) Admit(keyID string, limits WindowLimit, reservation int64, now time.Time) (ok bool, reason string) {
	if limits.TokensPerHour <= 0 && limits.TokensPerDay <= 0 {
		return true, ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	st := q.stateFor(keyID, now)
	if limits.TokensPerHour > 0 && (reservation > limits.TokensPerHour || st.hour > limits.TokensPerHour-reservation) {
		return false, "tokens_per_hour"
	}
	if limits.TokensPerDay > 0 && (reservation > limits.TokensPerDay || st.day > limits.TokensPerDay-reservation) {
		return false, "tokens_per_day"
	}
	return true, ""
}

// Settle adds the settled usage to the key's current windows (PLAN §39).
// Addition saturates so a single bogus value (or many large ones) cannot
// wrap a window counter negative and void the quota.
func (q *Quota) Settle(keyID string, tokens int64, now time.Time) {
	if tokens <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	st := q.stateFor(keyID, now)
	st.hour = satAdd(st.hour, tokens)
	st.day = satAdd(st.day, tokens)
}

// stateFor returns (creating if needed) the key's quota state, resetting
// the windows when they have rolled into a new fixed period.
func (q *Quota) stateFor(keyID string, now time.Time) *keyQuota {
	if q.states == nil {
		q.states = map[string]*keyQuota{}
	}
	h := hourKey(now)
	d := dayKey(now)
	st, ok := q.states[keyID]
	if !ok {
		st = &keyQuota{hourKey: h, dayKey: d}
		q.states[keyID] = st
		return st
	}
	if st.hourKey != h {
		st.hour = 0
		st.hourKey = h
	}
	if st.dayKey != d {
		st.day = 0
		st.dayKey = d
	}
	return st
}

// now returns the injected clock time, defaulting to the real time.
func (q *Quota) now() time.Time {
	if q.clock != nil {
		return q.clock()
	}
	return time.Now()
}

// replay reconstructs per-key token windows from a JSONL log (PLAN §40).
// It scans up to maxBytes from the end of the file (the longest active
// window is the recent tail) and aggregates only records that fall in the
// current fixed hour/day, so restart does not trivially reset quotas.
//
// A missing or empty file yields empty state. A read error returns it; the
// caller decides whether that is fatal (strict quota) or a warning.
func (q *Quota) Replay(path string, maxBytes int64) (err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if maxBytes <= 0 || size <= maxBytes {
		// Scan the whole file.
		return q.scan(f)
	}
	// Read only the tail: seek to size-maxBytes, then discard the first
	// (possibly partial) line.
	if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReader(f)
	// Discard up to and including the first newline to avoid a partial
	// leading line.
	if _, err := br.ReadBytes('\n'); err != nil && err != io.EOF {
		return err
	}
	return q.scanLines(br, q.now())
}

func (q *Quota) scan(f *os.File) error {
	return q.scanLines(bufio.NewReader(f), q.now())
}

func (q *Quota) scanLines(br *bufio.Reader, now time.Time) error {
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			q.replayLine(line, now)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// replayLine aggregates one record into the current windows. Malformed
// lines are skipped so a torn write never poisons replay. The charged
// amount (charged_tokens, falling back to total_tokens for legacy
// records) is what counts toward quota, so conservatively-charged unknown
// usage is restored on restart (PLAN §39, §40).
func (q *Quota) replayLine(line []byte, now time.Time) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return
	}
	tokens := r.ChargedTokens
	if tokens == 0 {
		tokens = r.TotalTokens
	}
	if r.KeyID == "" || tokens <= 0 {
		return
	}
	q.mu.Lock()
	st := q.stateFor(r.KeyID, now)
	// A record from an earlier window does not count toward the current
	// window.
	if hourKey(r.Time) == st.hourKey {
		st.hour = satAdd(st.hour, tokens)
	}
	if dayKey(r.Time) == st.dayKey {
		st.day = satAdd(st.day, tokens)
	}
	q.mu.Unlock()
}
