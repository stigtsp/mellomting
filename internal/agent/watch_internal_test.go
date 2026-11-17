package agent

import (
	"bufio"
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"mellomting/internal/discovery"
	"mellomting/internal/testsupport"
)

// A send to an attachment nobody reads any more gives up when its
// context ends, so close can wait for its helpers.
func TestSendGivesUpOnCancel(t *testing.T) {
	out := make(chan endEvent, 1)
	out <- endEvent{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { send(ctx, out, endEvent{reason: endBackendDown}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked past its context")
	}
}

// Retried model changes replace the backend watcher rather than add one:
// with a dead backend exactly one watcher reports it, and the helpers
// finish once the attachment stops.
func TestRetriedChangesKeepOneWatcher(t *testing.T) {
	restore := SetTimingsForTest(10 * time.Millisecond)
	defer restore()
	// Nothing listens on port 1: every probe fails at connect.
	be, _ := url.Parse("http://127.0.0.1:1")
	hctx, stop := context.WithCancel(context.Background())
	a := &attached{
		o:      Options{Backend: be, Log: testsupport.DiscardLogger()},
		st:     &stats{},
		events: make(chan endEvent, 1),
		hctx:   hctx,
		stop:   stop,
	}
	models := []discovery.Model{{ID: "m"}}
	a.watch(models)
	for range 5 {
		a.retryChange(models, time.Hour)
	}
	select {
	case ev := <-a.events:
		if ev.reason != endBackendDown {
			t.Fatalf("event %v, want backend down", ev.reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no backend-down event")
	}
	select {
	case ev := <-a.events:
		t.Fatalf("a second watcher reported %v", ev.reason)
	case <-time.After(200 * time.Millisecond):
	}
	done := make(chan struct{})
	go func() { a.stop(); a.helpers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("helpers did not finish after stop")
	}
}

// pipeAttachment is an attachment over one end of a pipe; closing the
// returned end's tracked connection is the edge hanging up.
func pipeAttachment(t *testing.T) *attached {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return &attached{tc: newTrackedConn(c1, bufio.NewReader(c1)), discovered: make(chan struct{})}
}

// A replacement attachment counts as accepted only once the edge has
// read its models and kept the connection open.
func TestAcceptedReplacement(t *testing.T) {
	restore := SetTimingsForTest(20 * time.Millisecond)
	defer restore()

	a := pipeAttachment(t)
	close(a.discovered)
	if err := a.accepted(context.Background()); err != nil {
		t.Fatalf("accepted: %v", err)
	}

	// The edge read the list, then refused it by closing.
	a = pipeAttachment(t)
	close(a.discovered)
	go func() { _ = a.tc.Close() }()
	if err := a.accepted(context.Background()); err == nil {
		t.Fatal("a connection closed after discovery was accepted")
	}

	// The edge closed before reading anything.
	a = pipeAttachment(t)
	_ = a.tc.Close()
	if err := a.accepted(context.Background()); err == nil {
		t.Fatal("a closed connection was accepted")
	}

	// The edge never read the list.
	a = pipeAttachment(t)
	if err := a.accepted(context.Background()); err == nil {
		t.Fatal("an attachment whose models were never read was accepted")
	}
}
