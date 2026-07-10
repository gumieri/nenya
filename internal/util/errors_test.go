package util

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsContextCanceled_Canceled(t *testing.T) {
	if !IsContextCanceled(context.Canceled) {
		t.Fatal("expected true for context.Canceled")
	}
}

func TestIsContextCanceled_DeadlineExceeded(t *testing.T) {
	if !IsContextCanceled(context.DeadlineExceeded) {
		t.Fatal("expected true for context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_WrappedCanceled(t *testing.T) {
	wrapped := fmt.Errorf("upstream failed: %w", context.Canceled)
	if !IsContextCanceled(wrapped) {
		t.Fatal("expected true for wrapped context.Canceled")
	}
}

func TestIsContextCanceled_WrappedDeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("upstream timeout: %w", context.DeadlineExceeded)
	if !IsContextCanceled(wrapped) {
		t.Fatal("expected true for wrapped context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_NilError(t *testing.T) {
	if IsContextCanceled(nil) {
		t.Fatal("expected false for nil error")
	}
}

func TestIsContextCanceled_GenericError(t *testing.T) {
	if IsContextCanceled(errors.New("connection refused")) {
		t.Fatal("expected false for generic error")
	}
}

func TestIsContextCanceled_MultiErrorCanceled(t *testing.T) {
	multi := errors.Join(context.Canceled, errors.New("some other error"))
	if !IsContextCanceled(multi) {
		t.Fatal("expected true for errors.Join containing context.Canceled")
	}
}

func TestIsContextCanceled_MultiErrorDeadlineExceeded(t *testing.T) {
	multi := errors.Join(context.DeadlineExceeded, errors.New("some other error"))
	if !IsContextCanceled(multi) {
		t.Fatal("expected true for errors.Join containing context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_MultiErrorNoContext(t *testing.T) {
	multi := errors.Join(errors.New("err1"), errors.New("err2"))
	if IsContextCanceled(multi) {
		t.Fatal("expected false for errors.Join without context errors")
	}
}
