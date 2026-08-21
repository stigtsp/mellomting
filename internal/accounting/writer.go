package accounting

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Writer appends accounting records to a JSONL file using one bounded
// queue (PLAN §42). Inference never blocks on the filesystem: Enqueue is
// non-blocking and drops the record (with a rate-limited alert) when the
// queue is full. Fsync policy controls durability.
type Writer struct {
	fh       *os.File
	q        chan Record
	fsync    string
	interval time.Duration
	log      *slog.Logger

	dropped atomic.Int64
	closed  atomic.Bool

	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
	// finalErr records the last filesystem error (final sync or close)
	// from the consumer goroutine. It is written before done is closed
	// and read by Close after <-done, so the happens-before of the
	// channel close makes the plain field safe (T-M11).
	finalErr  error
	lastAlert atomic.Int64
}

// WriterConfig carries the accounting settings (PLAN §42).
type WriterConfig struct {
	Path          string
	QueueSize     int
	FSync         string // "interval", "every", or "never"
	FSyncInterval time.Duration
	Log           *slog.Logger
}

// NewWriter opens (or creates) the JSONL log in append mode and starts
// the background consumer goroutine. It fails closed on any open error.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	fh, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open accounting log %q: %w", cfg.Path, err)
	}
	w := &Writer{
		fh:       fh,
		q:        make(chan Record, cfg.QueueSize),
		fsync:    cfg.FSync,
		interval: cfg.FSyncInterval,
		log:      cfg.Log,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if w.interval <= 0 {
		w.interval = 5 * time.Second
	}
	go w.run()
	return w, nil
}

// Enqueue submits a record for writing. It never blocks: if the bounded
// queue is full the record is dropped, a counter is incremented, and a
// rate-limited error is logged (PLAN §42). Records submitted after Close()
// has begun are counted as dropped rather than silently vanishing
// (T-M11).
func (w *Writer) Enqueue(r Record) {
	if w.closed.Load() {
		w.dropped.Add(1)
		return
	}
	select {
	case w.q <- r:
	default:
		w.dropped.Add(1)
		if w.alertDue(time.Now().Unix()) {
			w.log.Error("accounting queue overflow; records dropped",
				"dropped", w.dropped.Load())
		}
	}
}

// alertCadence is the minimum gap between overflow alerts, in seconds
// (PLAN §42).
const alertCadence = 30

// alertDue reports whether an overflow alert is due at Unix time now
// and, if so, claims the next alert window. lastAlert starts at 0, so
// the first drop fires an alert immediately; afterwards at most one
// alert fires per alertCadence. The CompareAndSwap uses the previously
// loaded value as the expected old value, so the first alert actually
// matches (T-M6: the CAS previously expected `now`, which never equals
// the initial 0, so the alert could never fire).
func (w *Writer) alertDue(now int64) bool {
	prev := w.lastAlert.Load()
	if now < prev {
		return false
	}
	return w.lastAlert.CompareAndSwap(prev, now+alertCadence)
}

// Dropped returns the cumulative number of dropped records.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// run consumes the queue, appends JSONL lines, and applies the fsync
// policy. It stops when the stop channel is closed and the queue drains.
func (w *Writer) run() {
	defer close(w.done)
	var lastFsync time.Time
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case r := <-w.q:
			if err := w.writeRecord(r); err != nil {
				w.log.Error("accounting write failed", "error_class", "accounting_write")
				w.dropped.Add(1)
			}
			if w.fsync == "every" {
				_ = w.fh.Sync()
			} else if w.fsync == "interval" && time.Since(lastFsync) >= w.interval {
				_ = w.fh.Sync()
				lastFsync = time.Now()
			}
		case <-w.stop:
			// Drain remaining queued records before finishing.
			for {
				select {
				case r := <-w.q:
					if err := w.writeRecord(r); err != nil {
						w.log.Error("accounting write failed", "error_class", "accounting_write")
						w.dropped.Add(1)
					}
				default:
					// Surface a final-sync (e.g. ENOSPC) or close error
					// through Close() instead of discarding it (T-M11).
					if w.finalErr == nil {
						w.finalErr = w.fh.Sync()
					}
					if w.finalErr == nil {
						w.finalErr = w.fh.Close()
					} else {
						_ = w.fh.Close()
					}
					return
				}
			}
		case <-ticker.C:
			if w.fsync == "interval" && time.Since(lastFsync) >= w.interval {
				_ = w.fh.Sync()
				lastFsync = time.Now()
			}
		}
	}
}

// writeRecord appends one JSONL line.
func (w *Writer) writeRecord(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := w.fh.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Close stops the consumer, drains the queue, syncs, and closes the file.
// It returns the final-sync or close error (T-M11), so an ENOSPC on the
// last flush is visible to the caller. Records that raced in after the
// drain are counted as dropped. Close is idempotent.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		close(w.stop)
		<-w.done
		// Records that arrived after the consumer finished can no longer
		// be written; count them dropped (T-M11).
		for {
			select {
			case <-w.q:
				w.dropped.Add(1)
			default:
				return
			}
		}
	})
	return w.finalErr
}
