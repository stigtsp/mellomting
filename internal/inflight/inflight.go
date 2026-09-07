// Package inflight tracks the requests a daemon is serving right now,
// so an operator can watch them (PLAN §43). It holds only what a
// request already carries — who is calling, what it is doing, how long
// it has been doing it — and forgets a request the moment it finishes,
// so its size is bounded by max_inflight_requests rather than by
// traffic.
//
// Nothing here is on the request's critical path except a mutex per
// state change, and every method is safe on a nil handle: a daemon with
// no registry runs the same code without a nil check at each call site.
package inflight

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// Phase is what a request is doing at the moment it is observed. The
// set mirrors the pipeline's real stages, so a stuck request says where
// it is stuck rather than only that it is slow.
type Phase string

const (
	// PhaseReading is reading the client's request body.
	PhaseReading Phase = "reading"
	// PhaseRouting is resolving the model and backend, and being
	// admitted against the key's limits and token quota.
	PhaseRouting Phase = "routing"
	// PhaseQueued is waiting for a backend concurrency slot: the
	// backend is busy, and this request has not been sent yet.
	PhaseQueued Phase = "queued"
	// PhaseWaiting has sent the request upstream and is waiting for the
	// model to produce its first byte.
	PhaseWaiting Phase = "waiting"
	// PhaseStreaming is relaying a response as it arrives.
	PhaseStreaming Phase = "streaming"
	// PhaseSending is writing a buffered response back to the client.
	PhaseSending Phase = "sending"
)

// Record is one live request, as observed. It is the wire form: the
// daemon serves it and `mellomting top` renders it, so both sides read
// one definition.
type Record struct {
	RequestID string `json:"request_id"`
	KeyID     string `json:"key_id"`
	KeyName   string `json:"key_name"`
	Remote    string `json:"remote"`
	UserAgent string `json:"user_agent,omitempty"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Phase     Phase  `json:"phase"`
	// AgeMillis is how long the request has been in the daemon.
	AgeMillis int64 `json:"age_ms"`
	BytesIn   int64 `json:"bytes_in"`
	BytesOut  int64 `json:"bytes_out"`
	// Tokens is what the backend has reported so far. It stays 0 until
	// a response carries usage, which for a stream is its final chunk —
	// an unfinished request has no token count to show, and inventing
	// one would be worse than showing none.
	Tokens int64 `json:"tokens"`
}

// Registry holds the live requests.
type Registry struct {
	mu   sync.Mutex
	live map[*Request]struct{}
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{live: make(map[*Request]struct{})}
}

// Request is one request's handle. Its immutable fields are set at
// Begin; the rest change as the request moves through the pipeline.
type Request struct {
	reg     *Registry
	started time.Time

	requestID, keyID, keyName   string
	remote, userAgent, endpoint string

	mu       sync.Mutex
	phase    Phase
	model    string
	backend  string
	bytesIn  int64
	bytesOut int64
	tokens   int64
}

// Begin records a request that has just been admitted. The returned
// handle must be ended, which is what keeps the registry bounded.
func (r *Registry) Begin(requestID, keyID, keyName, remote, userAgent, endpoint string) *Request {
	if r == nil {
		return nil
	}
	req := &Request{
		reg:       r,
		started:   time.Now(),
		requestID: requestID,
		keyID:     keyID,
		keyName:   keyName,
		remote:    remote,
		userAgent: userAgent,
		endpoint:  endpoint,
		phase:     PhaseReading,
	}
	r.mu.Lock()
	r.live[req] = struct{}{}
	r.mu.Unlock()
	return req
}

// End forgets the request.
func (q *Request) End() {
	if q == nil {
		return
	}
	q.reg.mu.Lock()
	delete(q.reg.live, q)
	q.reg.mu.Unlock()
}

// Phase records what the request is doing now.
func (q *Request) Phase(p Phase) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.phase = p
	q.mu.Unlock()
}

// Target records the model and backend once routing has chosen them.
func (q *Request) Target(model, backend string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.model, q.backend = model, backend
	q.mu.Unlock()
}

// Read records how many request bytes were accepted.
func (q *Request) Read(n int) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.bytesIn = int64(n)
	q.mu.Unlock()
}

// Wrote adds response bytes as they are relayed.
func (q *Request) Wrote(n int) {
	if q == nil || n <= 0 {
		return
	}
	q.mu.Lock()
	q.bytesOut += int64(n)
	q.mu.Unlock()
}

// Tokens records the token count a backend has reported.
func (q *Request) Tokens(n int64) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.tokens = n
	q.mu.Unlock()
}

// Snapshot returns the live requests, oldest first — the order an
// operator wants, because the request that has been running longest is
// the one worth looking at.
func (r *Registry) Snapshot() []Record {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	out := make([]Record, 0, len(r.live))
	for req := range r.live {
		out = append(out, req.record(now))
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Record) int {
		if c := cmp.Compare(b.AgeMillis, a.AgeMillis); c != 0 {
			return c
		}
		return cmp.Compare(a.RequestID, b.RequestID)
	})
	return out
}

func (q *Request) record(now time.Time) Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Record{
		RequestID: q.requestID,
		KeyID:     q.keyID,
		KeyName:   q.keyName,
		Remote:    q.remote,
		UserAgent: q.userAgent,
		Endpoint:  q.endpoint,
		Model:     q.model,
		Backend:   q.backend,
		Phase:     q.phase,
		AgeMillis: now.Sub(q.started).Milliseconds(),
		BytesIn:   q.bytesIn,
		BytesOut:  q.bytesOut,
		Tokens:    q.tokens,
	}
}
