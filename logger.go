package main

import (
	"log/slog"
	"os"
	"syscall"
)

func setupLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	var handler slog.Handler
	if isatty(os.Stderr.Fd()) {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func isatty(fd uintptr) bool {
	const _SYS_ISATTY = 16
	_, _, errno := syscall.Syscall(_SYS_ISATTY, fd, 0, 0)
	return errno == 0
}
