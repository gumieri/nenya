package infra

import (
	"fmt"
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

func SetLogLevel(level string) error {
	if level == "" {
		return nil
	}

	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return fmt.Errorf("invalid log level: %s (must be debug, info, warn, or error)", level)
	}

	logLevel.Set(slogLevel)

	var handler slog.Handler
	if isatty(os.Stderr.Fd()) {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel})
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return nil
}

func isatty(fd uintptr) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}