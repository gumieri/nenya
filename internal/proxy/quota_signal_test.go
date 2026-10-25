package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

// quotaAction builds an upstreamAction with the given status and body.
func quotaAction(status int, body string) upstreamAction {
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

// newQuotaTestLoop builds a retryLoop wired to a minimal gateway, as the
// concurrency tests do.
func newQuotaTestLoop(t *testing.T, p *Proxy, gw *gateway.NenyaGateway, agent string) *retryLoop {
	t.Helper()
	opts := forwardOptions{
		Targets:   []routing.UpstreamTarget{{Provider: "zai", Model: "glm-5.3", CoolKey: agent + "|x|zai|glm-5.3"}},
		AgentName: agent,
	}
	rl, err := newRetryLoop(p, gw, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), opts)
	if err != nil {
		t.Fatalf("newRetryLoop: %v", err)
	}
	return rl
}

// TestQuotaSignalStaysPerLoop pins AUD-13 (NENYA-229): the quota-exhaustion
// signal belongs to the retry loop that observed it. The old implementation
// stored the signal in ONE process-wide atomic; a concurrent request's loop
// then consumed the leftover flag on its next unrelated error and reported
// error_kind=quota_exhausted to a client whose upstream never said quota —
// or had its own quota signal cleared before it could read it.
func TestQuotaSignalStaysPerLoop(t *testing.T) {
	cfg := &config.Config{}
	gw := newTestGateway(cfg, map[string]*config.Provider{"zai": {Name: "zai"}})
	if gw.Logger == nil {
		gw.Logger = slog.Default()
	}
	p := &Proxy{}

	quotaErr := quotaAction(http.StatusTooManyRequests, `{"error":{"code":"1308","message":"quota reached, retry after reset"}}`)
	plainErr := quotaAction(http.StatusServiceUnavailable, `{"error":{"message":"upstream unavailable"}}`)

	targets := []routing.UpstreamTarget{{Provider: "zai", Model: "glm-5.3", CoolKey: "agent-a|x|zai|glm-5.3"}}

	// Step 1: request A's loop classifies its quota error through the shared
	// classify step. Pre-fix this PARKED the flag in a process-wide atomic
	// until some loop's next wrapper call consumed it.
	_, _, quotaA := p.handleUpstreamError(gw, 0, targets, targets[0], 5*time.Second, "agent-a", quotaErr)
	if !quotaA {
		t.Fatal("a quota 429 must return the quota signal to its caller")
	}

	// Step 2: request B, concurrently, hits a plain retryable failure. Pre-fix
	// this wrapper call loaded request A's parked flag, set B's
	// quotaExhausted, and cleared it — B's client was told quota_exhausted for
	// an upstream that never said quota, and A's own signal was lost.
	loopB := newQuotaTestLoop(t, p, gw, "agent-b")
	loopB.handleUpstreamError(0, loopB.opts.Targets[0], plainErr)
	if loopB.quotaExhausted {
		t.Fatal("loop B must not inherit request A's parked quota signal")
	}

	// Step 3: A's own loop, handling the error through its wrapper, keeps the
	// signal for its exhaustion report.
	loopA := newQuotaTestLoop(t, p, gw, "agent-a")
	loopA.handleUpstreamError(0, loopA.opts.Targets[0], quotaErr)
	if !loopA.quotaExhausted {
		t.Fatal("loop A must retain the quota signal for its exhaustion report")
	}

	// Step 4: the signal never outlives the loop that owns it.
	loopC := newQuotaTestLoop(t, p, gw, "agent-c")
	loopC.handleUpstreamError(0, loopC.opts.Targets[0], plainErr)
	if loopC.quotaExhausted {
		t.Fatal("the quota signal must not outlive the loop that observed it")
	}
}

// TestQuotaSignalStillClassifies is the contrast pin: the signal must still
// reach the loop that earned it, so the fix cannot be a blanket removal.
func TestQuotaSignalStillClassifies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governance.MinQuotaCooldownSeconds = 10
	gw := newTestGateway(cfg, map[string]*config.Provider{"zai": {Name: "zai"}})
	if gw.Logger == nil {
		gw.Logger = slog.Default()
	}
	p := &Proxy{}

	resetMs := time.Now().Add(2 * time.Second).UnixMilli()
	quotaErr := quotaAction(http.StatusTooManyRequests,
		`{"error":{"code":"1308","message":"`+strconv.FormatInt(resetMs, 10)+`"}}`)

	loop := newQuotaTestLoop(t, p, gw, "agent")
	loop.handleUpstreamError(0, loop.opts.Targets[0], quotaErr)
	if !loop.quotaExhausted {
		t.Fatal("quota signal lost: the loop must still retain its own quota flag")
	}
}
