package testutil

import (
	"io"
	"log/slog"
)

// NewTestLogger returns a silent *slog.Logger that discards all output.
// Use this in tests to suppress log noise while preserving structured logging paths.
func NewTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}
