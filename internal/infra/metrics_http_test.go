package infra

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestResponseWatcherUnwrapArmsDeadline pins the NENYA-155 enabler: the
// metrics wrapper must expose Unwrap() so http.ResponseController can arm a
// write deadline on the wrapped writer — production always wraps, so a
// regression here would silently no-op the whole feature.
func TestResponseWatcherUnwrapArmsDeadline(t *testing.T) {
	inner := newDeadlineAwareTestWriter()
	wrapped := &responseWatcher{ResponseWriter: inner, statusCode: 200}

	rc := http.NewResponseController(wrapped)
	if err := rc.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline through the wrapper: %v", err)
	}
	if inner.deadline.IsZero() {
		t.Fatal("the deadline never reached the wrapped writer (Unwrap missing?)")
	}

	future := time.Now().Add(time.Hour)
	if err := rc.SetWriteDeadline(future); err != nil {
		t.Fatalf("SetWriteDeadline through the wrapper: %v", err)
	}
	if !inner.deadline.Equal(future) {
		t.Fatalf("deadline = %v, want the armed %v", inner.deadline, future)
	}
}

// newDeadlineAwareTestWriter is the minimal deadline-aware inner writer.
type deadlineAwareTestWriter struct {
	deadline time.Time
}

func newDeadlineAwareTestWriter() *deadlineAwareTestWriter {
	return &deadlineAwareTestWriter{}
}

func (d *deadlineAwareTestWriter) Header() http.Header { return http.Header{} }

func (d *deadlineAwareTestWriter) WriteHeader(int) {}

func (d *deadlineAwareTestWriter) Write(p []byte) (int, error) {
	if !d.deadline.IsZero() && !time.Now().Before(d.deadline) {
		return 0, errTestWriteTimeout
	}
	return len(p), nil
}

func (d *deadlineAwareTestWriter) SetWriteDeadline(t time.Time) error {
	d.deadline = t
	return nil
}

var errTestWriteTimeout = errors.New("test write timeout")
