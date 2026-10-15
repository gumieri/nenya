package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/internal/testutil"
)

// sseFrameUpstream emits a 200 SSE stream from the given data frames WITHOUT
// renderable output — a pre-content refusal terminal (NENYA-191) or an
// in-stream rejection payload, depending on the caller's frames.
func sseFrameUpstream(calls *atomic.Int32, frames ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// contentThenRefusalUpstream emits renderable content and only then the
// refusal terminal (the post-content phase): the content was delivered, so
// the stream is terminal no matter the terminal's class.
func contentThenRefusalUpstream(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"partial answer"},"finish_reason":null}]}`+"\n\n")
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`+"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// servingUpstream is a plain healthy completion (shared by the failover tests).
func servingUpstream(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"served"},"finish_reason":"stop"}]}`)
	}))
}

// assertNoBreakerCharge asserts the target's circuit carries no failure: a
// policy refusal is not a provider fault, so cooldown/breaker state must stay
// untouched (NENYA-191). The pre-dispatch guard creates the circuit entry, so
// the failure count is the discriminating signal, not the entry's presence.
func assertNoBreakerCharge(t *testing.T, p *Proxy, key string) {
	t.Helper()
	snap := p.Gateway().AgentState.CBDetailedSnapshot()
	circuits, ok := snap["circuits"].(map[string]interface{})
	if !ok {
		t.Fatalf("no circuit snapshot: %v", snap)
	}
	c, ok := circuits[key].(map[string]interface{})
	if !ok {
		t.Fatalf("circuit %q missing from snapshot: %v", key, snap)
	}
	if charged(c["failure_count"]) {
		t.Fatalf("expected no breaker charge for %q, got %v", key, c)
	}
}

// charged reports whether a snapshot failure count is non-zero, whatever
// numeric shape the snapshot carried it in.
func charged(v any) bool {
	switch n := v.(type) {
	case int:
		return n != 0
	case int32:
		return n != 0
	case int64:
		return n != 0
	case uint32:
		return n != 0
	case uint64:
		return n != 0
	case float64:
		return n != 0
	default:
		// An unrecognized shape must fail LOUD, not pass vacuously: a
		// snapshot type change would otherwise silently void every
		// no-charge assertion.
		return true
	}
}

// TestPolicyRefusalBeforeContentFailsOver pins NENYA-191's pre-content phase:
// a 200 stream terminating with a refusal-class finish reason before any
// output fails over to the next target WITHOUT charging cooldown/breaker —
// the model declining content is not a provider fault.
func TestPolicyRefusalBeforeContentFailsOver(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := sseFrameUpstream(&firstCalls,
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`)
	defer refusing.Close()
	serving := servingUpstream(&secondCalls)
	defer serving.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, serving.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("expected the second target's answer, got: %s", rec.Body.String())
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the refusing target, got %d", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the serving target, got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyRefusalAnthropicShapeFailsOver pins the Anthropic-native refusal
// terminal (message_delta stop_reason refusal) through the same phase.
func TestPolicyRefusalAnthropicShapeFailsOver(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := sseFrameUpstream(&firstCalls,
		`{"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`)
	defer refusing.Close()
	serving := servingUpstream(&secondCalls)
	defer serving.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, serving.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("expected the second target's answer, got: %s", rec.Body.String())
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the refusing target, got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyErrorPayloadFailsOverWithoutCharge pins the error-payload variant:
// a safeguard/content-policy worded in-stream rejection fails over like any
// rejection, but without the provider-fault charge a non-policy rejection
// takes.
func TestPolicyErrorPayloadFailsOverWithoutCharge(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := sseFrameUpstream(&firstCalls,
		`{"error":{"type":"safeguard_error","message":"blocked by our content policy"}}`)
	defer refusing.Close()
	serving := servingUpstream(&secondCalls)
	defer serving.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, serving.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("expected the second target's answer, got: %s", rec.Body.String())
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the refusing target, got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyRefusalAfterContentIsTerminal pins the post-content phase: once
// renderable content was delivered, the refusal terminal is the outcome —
// the stream is never re-dispatched and no second target's output is
// appended.
func TestPolicyRefusalAfterContentIsTerminal(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := contentThenRefusalUpstream(&firstCalls)
	defer refusing.Close()
	never := servingUpstream(&secondCalls)
	defer never.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, never.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "partial answer") {
		t.Fatalf("expected the refusing target's content to be delivered, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "served") {
		t.Fatal("a post-content refusal must never re-dispatch onto the next target")
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("expected the second target to never be called, got %d", got)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the refusing target, got %d", got)
	}
	// The provider served a complete stream; the refusal is policy, not a
	// provider fault — no failure charge either way.
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyRefusalForwardedOnLastTarget pins the last-target phase: with no
// alternate target the refusal stream is forwarded so the client sees the
// refusal, and the target is still not charged a provider fault.
func TestPolicyRefusalForwardedOnLastTarget(t *testing.T) {
	var calls atomic.Int32
	refusing := sseFrameUpstream(&calls,
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"refusal"}]}`)
	defer refusing.Close()

	p := newBootstrapProxy(t, []string{refusing.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"refusal"`) {
		t.Fatalf("expected the refusal terminal to be forwarded, got: %s", rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 upstream call, got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestMessagesPolicyRefusalFailsOver pins that the /v1/messages route shares
// the chat failover taxonomy: the same pre-content refusal fails over there.
func TestMessagesPolicyRefusalFailsOver(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := sseFrameUpstream(&firstCalls,
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`)
	defer refusing.Close()
	serving := servingUpstream(&secondCalls)
	defer serving.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, serving.URL}, 65536)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/messages",
		`{"model":"test-agent","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("expected the second target's answer on /v1/messages, got: %s", rec.Body.String())
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the refusing target, got %d", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to the serving target, got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyRejectionForwardedOnLastTarget pins the [H]-class case: a
// policy-worded rejection on the FINAL target must be forwarded to the client
// without a same-target retry storm (the refusal is deterministic — retrying
// only burns the budget) and without a breaker charge.
func TestPolicyRejectionForwardedOnLastTarget(t *testing.T) {
	var calls atomic.Int32
	refusing := sseFrameUpstream(&calls,
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"error":{"type":"safeguard_error","message":"blocked by our content policy"}}`,
		`{"type":"response.completed"}`)
	defer refusing.Close()

	p := newBootstrapProxy(t, []string{refusing.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "content policy") {
		t.Fatalf("expected the policy rejection to be forwarded, got: %s", rec.Body.String())
	}
	// The FULL buffer is forwarded — handshake frames before and after the
	// decisive line arrive with it (the policy scan is prefix-limited; the
	// forward is not).
	for _, want := range []string{"response.created", "response.completed"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("expected %s in the forwarded stream, got: %s", want, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call (no same-target retry storm), got %d", got)
	}
	assertNoBreakerCharge(t, p, "test-agent:test-provider-0:test-model")
}

// TestPolicyRefusalMetrics pins the documented outcome labels: a pre-content
// refusal failover records both the bootstrap and early-error refusal
// outcomes (NENYA-191).
func TestPolicyRefusalMetrics(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	refusing := sseFrameUpstream(&firstCalls,
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`)
	defer refusing.Close()
	serving := servingUpstream(&secondCalls)
	defer serving.Close()

	p := newBootstrapProxy(t, []string{refusing.URL, serving.URL}, 65536)
	rec := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	var buf bytes.Buffer
	p.Gateway().Metrics.WritePrometheus(&buf)
	out := buf.String()
	for _, want := range []string{
		`outcome="refusal_failover"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in metrics, got:\n%s", want, out)
		}
	}
	// Both counters must carry the refusal outcome (the bootstrap verdict and
	// the early-error record), not just one.
	if strings.Count(out, `outcome="refusal_failover"`) < 2 {
		t.Fatalf("expected refusal_failover on both bootstrap and early-error counters, got:\n%s", out)
	}
}

// TestParseProviderError_PolicyRefusal pins the LIVE ErrorKind mapping: a
// relayed upstream 4xx worded as a content-policy rejection carries
// error_kind=policy_refusal (request-scoped, failover-eligible), not
// invalid_request.
func TestParseProviderError_PolicyRefusal(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantType ErrorType
	}{
		{
			name:     "content policy 400",
			status:   400,
			body:     `{"error":{"message":"violates our content policy"}}`,
			wantType: ErrorTypePolicyRefusal,
		},
		{
			name:     "safeguard type with an empty message",
			status:   422,
			body:     `{"error":{"type":"safeguard_error"}}`,
			wantType: ErrorTypePolicyRefusal,
		},
		{
			name:     "a generic 400 stays invalid_request",
			status:   400,
			body:     `{"error":{"message":"invalid parameter"}}`,
			wantType: ErrorTypeInvalidRequest,
		},
		{
			name:     "a 403 stays auth even with policy wording",
			status:   403,
			body:     `{"error":{"message":"content policy"}}`,
			wantType: ErrorTypeAuthentication,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseProviderError("test-provider", tc.status, []byte(tc.body), nil)
			if got.Type != tc.wantType {
				t.Fatalf("Type = %q, want %q", got.Type, tc.wantType)
			}
		})
	}
}
