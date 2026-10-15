package proxy

import (
	"time"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

// NENYA-153 — slow calls as circuit-breaker failures.
//
// A provider that answers but takes minutes never trips a breaker that only
// counts errors, so traffic keeps pooling on a degraded target. With a
// configured threshold, a call slower than it counts as a breaker failure while
// the answer still reaches its caller: only the breaker sees the failure. The
// streaming clock is the time-to-first-effective-output (NENYA-178), so a
// slow-thinking model is not punished for reasoning time; buffered calls use the
// whole call (dispatch to body read).
//
// Scope: the chat paths (streaming and buffered) and the buffered MCP replay
// path charge slow calls. The remaining passthrough endpoints
// (/v1/embeddings, /v1/responses, /proxy/*, …) dispatch upstream without
// recording any breaker outcome, so a degraded target reached only through them
// is still not broken out of.

// slowCallThresholdFor resolves the effective slow-call threshold for an agent:
// the per-agent override wins over the global governance value, and 0 means off.
func slowCallThresholdFor(gw *gateway.NenyaGateway, agentName string) time.Duration {
	if gw == nil {
		return 0
	}
	global := gw.Config.Governance.EffectiveSlowCallThreshold()
	agent, ok := gw.Config.Agents[agentName]
	if !ok {
		return global
	}
	return agent.EffectiveSlowCallThreshold(global)
}

// slowCallSignal records the elapsed time to the first effective output of a
// streamed attempt, so the terminal outcome can count an over-threshold first
// token as a breaker failure. It carries the threshold it was created with, so
// the observation and the charge can never disagree.
//
// No lock: the reader's first-output hook runs synchronously on the goroutine
// that drains the stream, and the terminal decision runs on that same goroutine
// once the copy returns.
type slowCallSignal struct {
	threshold time.Duration
	elapsed   time.Duration
	observed  bool
}

// newSlowCallSignal returns a signal for one logical request, or nil when the
// threshold is off (callers then skip the bookkeeping entirely).
func newSlowCallSignal(threshold time.Duration) *slowCallSignal {
	if threshold <= 0 {
		return nil
	}
	return &slowCallSignal{threshold: threshold}
}

// observe records the time to the first effective output. Only the first
// observation counts: a continuation attempt re-dispatches mid-generation, so
// its "first output" is not the logical request's first token.
func (s *slowCallSignal) observe(elapsed time.Duration) {
	if s == nil || s.observed {
		return
	}
	s.observed, s.elapsed = true, elapsed
}

// slow reports whether a first effective output was observed and exceeded the
// threshold. An unobserved signal is never slow: a stream that produced no
// output has no first-token latency to judge.
func (s *slowCallSignal) slow() bool {
	return s != nil && s.observed && s.elapsed > s.threshold
}

// recordSlowCall resolves the agent's threshold and charges the call when it
// exceeds it. It reports whether the call was counted.
func recordSlowCall(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, elapsed, cooldown time.Duration) bool {
	return recordSlowCallAt(gw, target, agentName, slowCallThresholdFor(gw, agentName), elapsed, cooldown)
}

// recordSlowCallAt charges an over-threshold call to the breaker without touching
// the response the client receives. A disabled threshold, an unmeasured (zero)
// elapsed time, or a target without a breaker key is a no-op.
//
// Ordering matters: callers must use this instead of RecordSuccess, never after
// it. A success would clear the consecutive-failure streak, and in the half-open
// state it would close the circuit — which is exactly what a slow call must not
// do.
func recordSlowCallAt(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, threshold, elapsed, cooldown time.Duration) bool {
	if gw == nil || gw.AgentState == nil || target.CoolKey == "" {
		return false
	}
	if threshold <= 0 || elapsed <= threshold {
		return false
	}
	gw.AgentState.RecordFailure(target, cooldown)
	gw.Metrics.RecordSlowCall(target.Model, agentName, target.Provider)
	// AgentState.RecordFailure does not emit the CB counter (only the
	// classified RecordFailureWithStatus does), so a slow call counts here
	// explicitly: nenya_cb_failures_total then covers both classified upstream
	// failures and charged slow calls.
	gw.Metrics.RecordCBFailure(target.CoolKey)
	if gw.Logger != nil {
		gw.Logger.Debug("slow call counted as a circuit-breaker failure (NENYA-153)",
			"agent", agentName, "model", target.Model, "provider", target.Provider,
			"elapsed", elapsed.String(), "threshold", threshold.String())
	}
	return true
}

// recordBufferedOutcome records the breaker outcome of a served buffered call:
// a slow call is charged as a failure, a fast one as a success. The success
// matters for recovery — without it a half-open circuit would consume its probe
// slots and never close, since buffered requests are the only traffic some
// callers send.
//
// The helper owns the clock and takes both instants — dispatchAt and the instant
// the upstream body was fully read — so the measurement is the call itself and
// never includes egress screening, JSON encoding or the client write (a stalled
// client must not make a healthy provider look slow). A zero dispatchAt (an
// action built without one) records the success without a slow-call
// measurement — fail-open by design, since an unmeasured call must not be
// charged on a bogus elapsed time; callers that build actions must set it.
func recordBufferedOutcome(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, dispatchAt, bodyReadAt time.Time, cooldown time.Duration) {
	if gw == nil || gw.AgentState == nil || target.CoolKey == "" {
		return
	}
	if !dispatchAt.IsZero() && recordSlowCall(gw, target, agentName, bodyReadAt.Sub(dispatchAt), cooldown) {
		return
	}
	gw.AgentState.RecordSuccess(target.CoolKey)
}
