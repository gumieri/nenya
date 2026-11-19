package proxy

// NENYA-332 pin: when reading an upstream ERROR body fails mid-read (network
// reset), the truncation must be logged loudly — a truncated body silently
// feeding retryable-phrase classification can flip retryable↔terminal
// decisions with no observable trace.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/internal/routing"
)

// errBodyReader fails every read after zero bytes (a mid-body reset).
type errBodyReader struct{ err error }

func (r *errBodyReader) Read([]byte) (int, error) { return 0, r.err }
func (r *errBodyReader) Close() error             { return nil }

func TestHandleActionErrorLogsErrorBodyReadFailure(t *testing.T) {
	p, _ := newTestProxy(t)
	logBuf := &bytes.Buffer{}
	gw := newTestGateway(nil, nil)
	gw.Logger = slog.New(slog.NewTextHandler(logBuf, nil))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	opts := forwardOptions{
		Targets:   []routing.UpstreamTarget{{Provider: "test-provider", Model: "test-model"}},
		AgentName: "read-fail-agent",
	}
	rl, err := newRetryLoop(p, gw, w, r, opts)
	if err != nil {
		t.Fatalf("newRetryLoop: %v", err)
	}

	readErr := errors.New("reset mid-body")
	action := upstreamAction{
		Kind:   actionError,
		Resp:   &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: &errBodyReader{err: readErr}},
		Cancel: func() {},
	}
	_ = rl.handleActionError(0, opts.Targets[0], action)

	logs := logBuf.String()
	if !strings.Contains(logs, "error body") || !strings.Contains(logs, "reset mid-body") {
		t.Fatalf("a failed error-body read must be logged loudly (mention the read error); logs:\n%s", logs)
	}
}

// TestHandleActionError_SkipsWarnOnBenignCancellation pins the round-2
// refinement: a context.Canceled body-read failure is benign teardown
// (client disconnect drains) — no truncated-classification warning.
func TestHandleActionError_SkipsWarnOnBenignCancellation(t *testing.T) {
	p, _ := newTestProxy(t)
	logBuf := &bytes.Buffer{}
	gw := newTestGateway(nil, nil)
	gw.Logger = slog.New(slog.NewTextHandler(logBuf, nil))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	opts := forwardOptions{
		Targets:   []routing.UpstreamTarget{{Provider: "test-provider", Model: "test-model"}},
		AgentName: "cancel-drain-agent",
	}
	rl, err := newRetryLoop(p, gw, w, r, opts)
	if err != nil {
		t.Fatalf("newRetryLoop: %v", err)
	}

	action := upstreamAction{
		Kind:   actionError,
		Resp:   &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: &errBodyReader{err: context.Canceled}},
		Cancel: func() {},
	}
	_ = rl.handleActionError(0, opts.Targets[0], action)

	if strings.Contains(logBuf.String(), "truncated body") {
		t.Fatalf("a context.Canceled body-read failure is benign teardown — no warn; logs:\n%s", logBuf.String())
	}
}
