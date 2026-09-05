// Package testsupport holds fixtures shared by the test suites.
//
// The internal test files are whitebox (package proxy, package backend,
// …), so a shared helper cannot live in a _test.go file and must be a
// normal package. It therefore must not import any package whose own
// whitebox tests use it: config and auth are fine, proxy, backend,
// httpapi and tlsconfig are not.
package testsupport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// DiscardLogger returns a logger above every level, so tests that need a
// *slog.Logger produce no output.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

// UnixClient returns an HTTP client that reaches a unix-socket listener.
// The URL's host is ignored; every request goes to sock.
func UnixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// FakeModelsServer serves an OpenAI-shaped /v1/models listing of ids and
// 404s every other path, so a test that walks off the endpoint fails
// rather than silently succeeding.
func FakeModelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%q,"object":"model"}`, id)
		}
		b.WriteString(`]}`)
		_, _ = fmt.Fprint(w, b.String())
	}))
	t.Cleanup(ts.Close)
	return ts
}
