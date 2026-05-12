package infra

import (
	"strings"
	"testing"
)

func TestSetupLogger_InfoLevel(t *testing.T) {
	logger := SetupLogger(false)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestSetupLogger_DebugLevel(t *testing.T) {
	logger := SetupLogger(true)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestSetLogLevel_Valid(t *testing.T) {
	tests := []struct {
		level string
	}{
		{"debug"},
		{"info"},
		{"warn"},
		{"error"},
		{""},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			err := SetLogLevel(tc.level)
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tc.level, err)
			}
		})
	}
}

func TestSetLogLevel_Invalid(t *testing.T) {
	tests := []string{
		"trace",
		"fatal",
		"INFO",
	}
	for _, level := range tests {
		t.Run(level, func(t *testing.T) {
			err := SetLogLevel(level)
			if err == nil {
				t.Error("expected error for invalid level")
			}
			if !strings.Contains(err.Error(), "invalid log level") {
				t.Errorf("expected 'invalid log level' in error, got %v", err)
			}
		})
	}
}

func TestIsatty_DevNull(t *testing.T) {
	got := isatty(0)
	if got {
		t.Log("stdin is a tty; this test may not be meaningful in non-tty env")
	}
}
