package infra

import (
	"log/slog"
	"os"
	"syscall"
)

var logLevel slog.LevelVar

func SetupLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return SetupLoggerWithLevel(level)
}

func SetupLoggerWithLevel(level slog.Level) *slog.Logger {
	logLevel.Set(level)

	var handler slog.Handler
	if isatty(os.Stderr.Fd()) {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel})
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func isatty(fd uintptr) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}
