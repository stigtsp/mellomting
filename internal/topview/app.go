package topview

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/topview/term"
)

// Source is where top reads the daemon: the admin socket, or the
// simulator.
type Source interface {
	Stats(ctx context.Context, since int64) (adminapi.Stats, error)
}

const (
	// pollTimeout bounds one read of the daemon, so a hung daemon shows
	// the banner instead of freezing the screen.
	pollTimeout = 3 * time.Second
	// escWait is how long a lone ESC waits for the rest of a sequence.
	escWait = 50 * time.Millisecond
	// usageEvery is how often the accounting log is tailed.
	usageEvery = 10 * time.Second
)

// poll reads the daemon once into m.
func poll(ctx context.Context, src Source, m *Model) {
	st, err := src.Stats(ctx, m.Since())
	if err != nil {
		m.Fail(err)
		return
	}
	m.Apply(st)
}

type result struct {
	st  adminapi.Stats
	err error
}

// Run is the interactive viewer. Keys are read on their own goroutine
// and the daemon is polled on another, so a key press redraws at once
// whatever the daemon is doing; all state changes and drawing happen
// here, under the session's panic guard.
func Run(ctx context.Context, src Source, sess *term.Session, opt Options) (err error) {
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	m := NewModel(opt.Loc)
	m.SetUsage(opt.Usage, opt.UsageErr, opt.BackendServer)
	ui := NewUI(opt)

	input := make(chan []byte, 32)
	go readInput(opt.In, input)
	results := make(chan result, 1)
	resize := make(chan os.Signal, 1)
	term.NotifyResize(resize)
	ticker := time.NewTicker(opt.Interval)
	defer ticker.Stop()
	usageTick := time.NewTicker(usageEvery)
	defer usageTick.Stop()

	polling := false
	startPoll := func() {
		if polling {
			return
		}
		polling = true
		since := m.Since()
		go func() {
			var r result
			defer func() {
				if p := recover(); p != nil {
					r = result{err: fmt.Errorf("poll: %v", p)}
				}
				results <- r
			}()
			pctx, cancel := context.WithTimeout(ctx, pollTimeout)
			defer cancel()
			r.st, r.err = src.Stats(pctx, since)
		}()
	}
	draw := func() {
		w, h := sess.Size()
		ui.Height = h
		_ = sess.Draw(Frame(m, ui, w, h))
	}

	sess.Guard(func() {
		startPoll()
		draw()
		var rest []byte
		var esc <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-input:
				if !ok {
					return // the terminal went away
				}
				buf := append(rest, b...)
				// Take everything already read, so a held key does not
				// queue frames behind it.
				for more := true; more; {
					select {
					case b2, ok2 := <-input:
						if !ok2 {
							return
						}
						buf = append(buf, b2...)
					default:
						more = false
					}
				}
				var keys []term.Key
				keys, rest = term.Decode(buf)
				esc = nil
				if len(rest) > 0 {
					esc = time.After(escWait)
				}
				for _, k := range keys {
					ui.handle(k, m)
				}
				if ui.Quit {
					return
				}
				draw()
			case <-esc:
				for _, k := range term.Flush(rest) {
					ui.handle(k, m)
				}
				rest, esc = nil, nil
				draw()
			case r := <-results:
				polling = false
				if ui.Paused {
					break
				}
				if r.err != nil {
					m.Fail(r.err)
				} else if m.Apply(r.st) {
					startPoll() // the daemon restarted: fetch its whole window
				}
				draw()
			case <-ticker.C:
				if !ui.Paused {
					startPoll()
				}
			case <-resize:
				sess.Redraw()
				draw()
			case <-usageTick.C:
				if m.usage != nil && !ui.Paused {
					if err := m.usage.Refresh(time.Now()); err != nil {
						m.usageErr = "cannot read the accounting log: " + describeFile(err)
						m.usage = nil
					}
					draw()
				}
			}
		}
	})
	return nil
}

func describeFile(err error) string {
	switch {
	case os.IsPermission(err):
		return "permission denied"
	case os.IsNotExist(err):
		return "it is gone"
	}
	return "read failed"
}

// readInput passes terminal input to Run until it fails.
func readInput(in io.Reader, out chan<- []byte) {
	if in == nil {
		return // no terminal input: run until ctx ends
	}
	defer close(out)
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			out <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			return
		}
	}
}
