// Package logging builds the structured operational logger for Mellomting.
//
// Operational logs are structured JSON on stdout/stderr (PLAN §43).
// Callers log only sanitized operational fields: never prompts,
// responses, API keys, backend credentials, or raw backend error bodies.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Log formats and levels (PLAN §76).
const (
	FormatJSON = "json"
	FormatText = "text"

	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// ParseLevel parses a configured log level name.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo:
		return slog.LevelInfo, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
}

// ParseFormat parses a configured log format name.
func ParseFormat(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case FormatJSON:
		return FormatJSON, nil
	case FormatText:
		return FormatText, nil
	}
	return "", fmt.Errorf("unknown log format %q (want json or text)", s)
}

// New builds a slog.Logger writing to w at the given level and format.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	normalized, err := ParseFormat(format)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch normalized {
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
}
