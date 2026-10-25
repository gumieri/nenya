package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/routing"
)

// cancellessAction builds an upstreamAction with NO cancel func — as guard
// actions and several test fixtures do.
func cancellessAction(status int, body string) upstreamAction {
	action := upstreamAction{
		resp: &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
	action.body, _ = io.ReadAll(action.resp.Body)
	return action
}

// TestHandleUpstreamErrorTerminalPathsWithoutCancel pins AUD-49 (NENYA-235):
// the terminal error paths must not require the action to carry a cancel
// func. Pre-fix a `defer action.cancel()` sat on exactly those paths, so any
// caller (or test) passing a cancel-less action panicked with a nil-func
// call. The contract is that CALLERS own cancellation: the chat loop cancels
// before entry (handleActionError, after the error body is consumed — its
// comment already notes later cancels are harmless), the MCP buffered loop
// right after return.
func TestHandleUpstreamErrorTerminalPathsWithoutCancel(t *testing.T) {
	cfg := &config.Config{}
	gw := newTestGateway(cfg, map[string]*config.Provider{"prov": {Name: "prov"}})
	p := &Proxy{}

	targets := []routing.UpstreamTarget{
		{Provider: "prov", Model: "m", CoolKey: "a|p|m"},
		{Provider: "prov2", Model: "m", CoolKey: "a|p2|m"},
	}

	t.Run("model not found fails over", func(t *testing.T) {
		action := cancellessAction(http.StatusNotFound, `{"error":{"message":"model not found"}}`)
		signal, _, _ := p.handleUpstreamError(gw, 0, targets, targets[0], 0, "agent", action)
		if signal != retrySignalContinue {
			t.Fatalf("model-not-found with a later target must continue, got %v", signal)
		}
	})

	t.Run("non-retryable fails over", func(t *testing.T) {
		action := cancellessAction(http.StatusBadRequest, `{"error":{"message":"invalid request"}}`)
		signal, _, _ := p.handleUpstreamError(gw, 0, targets, targets[0], 0, "agent", action)
		if signal != retrySignalContinue {
			t.Fatalf("non-retryable with a later target must continue, got %v", signal)
		}
	})

	t.Run("last target done", func(t *testing.T) {
		action := cancellessAction(http.StatusBadRequest, `{"error":{"message":"invalid request"}}`)
		signal, _, _ := p.handleUpstreamError(gw, 1, targets[:1], targets[0], 0, "agent", action)
		if signal != retrySignalDone {
			t.Fatalf("non-retryable with no later target must be done, got %v", signal)
		}
	})
}
