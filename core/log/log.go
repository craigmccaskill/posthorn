// Package log provides structured logging primitives for Posthorn.
//
// v1.0 wraps log/slog (stdlib) with a JSON handler and a small set of
// helpers: New() constructs a logger from a config-shaped string level,
// SubmissionID generates the UUIDv4 propagated through every log line
// for a single request (NFR8), and Discard returns a no-op logger for
// tests and embedded usage.
//
// Per ADR-2-revised: slog in core (zero deps for the logging machinery
// itself; uuid is the one external dep, ~200 LOC well-known package).
// The Caddy adapter uses Caddy's zap logger; bridging happens at the
// adapter boundary.
package log

import (
	"io"
	"log/slog"
	"os"

	"github.com/google/uuid"
)

// New returns a slog.Logger writing JSON lines to stdout at the given
// level. Empty level defaults to info. Unknown levels also default to
// info (operator typos shouldn't silently drop logs to debug or error).
func New(level string) *slog.Logger {
	return NewWithWriter(os.Stdout, level)
}

// NewWithWriter is like [New] but writes to the given io.Writer.
// Useful for tests that want to capture log output.
func NewWithWriter(w io.Writer, level string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	return slog.New(handler)
}

// Discard returns a logger that drops everything. Use in tests where
// log output is incidental, or as a default before the operator's
// configured logger is built.
func Discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{}))
}

// SubmissionID returns a fresh UUIDv4 string for use as the
// per-request submission ID (NFR8). Errors are extremely unlikely
// (only on entropy-source failure); the function panics in that case
// because a server that can't generate UUIDs is fundamentally broken
// and crash-loop-restart is the right behavior.
func SubmissionID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		panic("log: uuid.NewRandom failed: " + err.Error())
	}
	return id.String()
}

// parseLevel converts a config-shaped string to a slog.Level.
// Unknown / empty values fall back to LevelInfo.
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}
