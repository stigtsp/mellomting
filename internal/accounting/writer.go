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

	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
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
// rate-limited error is logged (PLAN §42).
func (w *Writer) Enqueue(r Record) {
	select {
	case w.q <- r:
	default:
		w.dropped.Add(1)
		if now := time.Now().Unix(); now >= w.lastAlert.Load() {
			if w.lastAlert.CompareAndSwap(now, now+30) {
				w.log.Error("accounting queue overflow; records dropped",
					"dropped", w.dropped.Load())
			}
		}
	}
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
						w.dropped.Add(1)
					}
				default:
					_ = w.fh.Sync()
					_ = w.fh.Close()
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
func (w *Writer) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
	return err
}
