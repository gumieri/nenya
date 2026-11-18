package infra

import (
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

func TestIsatty_DevNull(t *testing.T) {
	got := isatty(0)
	if got {
		t.Log("stdin is a tty; this test may not be meaningful in non-tty env")
	}
}
