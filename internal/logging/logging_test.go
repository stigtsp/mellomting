package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{in: "debug", want: slog.LevelDebug, ok: true},
		{in: "info", want: slog.LevelInfo, ok: true},
		{in: "warn", want: slog.LevelWarn, ok: true},
		{in: "error", want: slog.LevelError, ok: true},
		{in: "  Warn ", want: slog.LevelWarn, ok: true},
		{in: "", ok: false},
		{in: "verbose", ok: false},
		{in: "warning", ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLevel(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseLevel(%q) error = %v, want nil", tc.in, err)
				}
				if got != tc.want {
					t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
				}
			} else if err == nil {
				t.Fatalf("ParseLevel(%q) succeeded, want error", tc.in)
			}
		})
	}
}

func TestNewJSONEmitsStructuredFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, LevelInfo, FormatJSON)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("configuration loaded", "path", "/etc/mellomting/config.yaml", "keys", 3)

	out := buf.String()
	for _, want := range []string{`"level":"INFO"`, `"msg":"configuration loaded"`, `"path":"/etc/mellomting/config.yaml"`, `"keys":3`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log output %q does not contain %s", out, want)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, LevelError, FormatJSON)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("suppressed")
	if buf.Len() != 0 {
		t.Fatalf("info log written despite error level: %s", buf.String())
	}
	logger.Error("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Fatalf("error log missing: %s", buf.String())
	}
}

func TestNewRejectsBadFormatAndLevel(t *testing.T) {
	t.Parallel()

	if _, err := New(&bytes.Buffer{}, "debug", "yaml"); err == nil {
		t.Fatal("bad format accepted")
	}
	if _, err := New(&bytes.Buffer{}, "loud", "json"); err == nil {
		t.Fatal("bad level accepted")
	}
}
