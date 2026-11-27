package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// NENYA-346 (Module A Phase 030): the /v1/responses passthrough gains the
// output-side security pipeline. The chat routes run the egress guards
// (ExfilGuard URL policy, secret/entropy redaction, canary tripwire) and the
// streamed tool-call argument assembler with its enforcing guards on every
// response; this file extends the same defenses to the Responses endpoint,
// which previously copied upstream bytes to the client untouched. Default
// ON; governance.responses_pipeline.enabled=false is the kill switch that
// restores the raw passthrough.

// maxResponsesBufferedBodyBytes bounds the buffered relay's scan buffer: the
// request-side body limit is the operator's declared ceiling for a single
// exchange (config.DefaultMaxBodyBytes when unset), so a response body
// beyond it is refused rather than scanned in an unbounded allocation. The
// value has no configured upper bound, so it is clamped below MaxInt64
// before the +1 truncation probe (CWE-190): a negative limit would make
// LimitReader yield an empty body that relays as a 200.
func maxResponsesBufferedBodyBytes(gw *gateway.NenyaGateway) int64 {
	limit := gw.Config.Server.MaxBodyBytes
	if limit <= 0 {
		limit = config.DefaultMaxBodyBytes
	}
	if limit == math.MaxInt64 {
		limit = math.MaxInt64 - 1
	}
	return limit
}

// injectResponsesCanary generates the per-request canary and appends its
// system-context marker to the create request's instructions channel — the
// Responses system-context field and the twin of the chat path's trailing
// system message (pipeline.InjectCanary). Called AFTER agent scoping (the
// marker must not participate in authorization) and BEFORE dispatch (the
// idempotency key binds the marker-bearing body, NENYA-165), so a re-dispatch
// of the same body repeats the same marker. Every member value other than
// `instructions` keeps its JSON SEMANTICS untouched (json.RawMessage —
// integers keep precision, no float64 widening); the re-encode still
// normalizes the wire: insignificant whitespace compacts, HTML escaping
// re-encodes, and top-level member order sorts — the body is semantically
// identical, not byte-identical (documented; the idempotency key binds the
// dispatched spelling, so retries stay consistent). Returns the canary
// state, the rewritten body, and whether injection happened; a body that
// cannot carry the marker (non-object JSON, non-string instructions)
// disarms the tripwire for the request (an honest no-watch beats a watcher
// whose token the upstream never saw) and the original body is dispatched
// unchanged.
func injectResponsesCanary(gw *gateway.NenyaGateway, body []byte) (pipeline.CanarResult, []byte, bool) {
	cfg := gw.Config.Governance.Canary
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return pipeline.CanarResult{}, body, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		gw.Logger.Warn("canary enabled but the responses body is not a JSON object; tripwire disarmed for this request")
		return pipeline.CanarResult{}, body, false
	}
	canary := pipeline.GenerateCanary()
	marker := pipeline.CanaryMarker(canary)
	merged, ok := mergeInstructionsMarker(fields, marker)
	if !ok {
		// A non-string instructions field is provider-defined structure;
		// rewriting it could corrupt the request semantics.
		gw.Logger.Warn("canary enabled but instructions is not a string; tripwire disarmed for this request")
		return pipeline.CanarResult{}, body, false
	}
	fields["instructions"] = merged
	encoded, err := json.Marshal(fields)
	if err != nil {
		gw.Logger.Warn("canary enabled but the rewritten body failed to marshal; tripwire disarmed for this request", "err", err)
		return pipeline.CanarResult{}, body, false
	}
	action := cfg.Action
	if action == "" {
		action = config.CanaryActionBlock
	}
	return pipeline.CanarResult{Token: canary, Action: action}, encoded, true
}

// mergeInstructionsMarker merges the canary marker into the instructions
// member: appended to the existing string, or set when the member is
// absent. Returns the replacement raw value, or ok=false when the member
// exists but is not a JSON string (fail closed — see
// injectResponsesCanary).
func mergeInstructionsMarker(fields map[string]json.RawMessage, marker string) ([]byte, bool) {
	if raw, present := fields["instructions"]; present {
		var existing string
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, false
		}
		merged, err := json.Marshal(existing + "\n" + marker)
		if err != nil {
			return nil, false
		}
		return merged, true
	}
	merged, err := json.Marshal(marker)
	if err != nil {
		return nil, false
	}
	return merged, true
}

// relayResponsesUpstream relays one upstream response to the client: the
// guarded pipeline when the responses pipeline is enabled (default ON;
// governance.responses_pipeline.enabled=false restores the raw passthrough
// copy — the kill switch).
func (p *Proxy) relayResponsesUpstream(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, resp *http.Response, providerName, model, agentName string, canary pipeline.CanarResult, logger *slog.Logger) {
	if gw.Config.Governance.EffectiveResponsesPipelineEnabled() {
		p.relayResponsesGuarded(gw, w, r, resp, providerName, model, agentName, canary, logger)
		return
	}
	writeUpstreamResponse(r.Context(), w, resp, logger)
}

// relayResponsesGuarded runs the guarded relay for one upstream response:
// 2xx bodies are scanned (streamed frames inline, buffered bodies
// wholesale); non-2xx error bodies pass through unscanned — their fidelity
// is diagnostic, and model text lives in 2xx payloads.
func (p *Proxy) relayResponsesGuarded(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, resp *http.Response, providerName, model, agentName string, canary pipeline.CanarResult, logger *slog.Logger) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		writeUpstreamResponse(r.Context(), w, resp, logger)
		return
	}
	if isResponsesEventStream(resp) {
		p.relayResponsesStream(gw, w, r, resp, providerName, model, agentName, canary, logger)
		return
	}
	p.relayResponsesBuffered(gw, w, r, resp, providerName, model, agentName, canary, logger)
}

// isResponsesEventStream reports whether the upstream response is an SSE
// stream (the create endpoint with stream:true, or an event-bearing
// content type the provider chose).
func isResponsesEventStream(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// relayResponsesStream pipes the upstream Responses SSE stream through the
// transforming reader with the full guard set: the text-path egress guards
// (secret/policy filter, ExFilGuard URL policy, entropy redactor, canary
// tripwire) scan every frame inline BEFORE the frame is forwarded, and the
// tool-call argument assembler feeds the dedicated enforcing guards
// (identical wiring to the chat path). A guard verdict suppresses the
// carrying frame and terminates the stream with a structured Responses
// error event; bytes already forwarded cannot be recalled (stop-loss).
func (p *Proxy) relayResponsesStream(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, resp *http.Response, providerName, model, agentName string, canary pipeline.CanarResult, logger *slog.Logger) {
	// Headers copy verbatim except the length/encoding pair: guard rewrites
	// and frame suppression change the byte count, and SSE carries no
	// client-side length expectation (the upstream is dispatched with
	// Accept-Encoding: identity, NENYA-189 buildUpstreamRequest).
	routing.CopyHeaders(resp.Header, w.Header())
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	reader := stream.NewSSETransformingReader(resp.Body, nil, r.Context())
	reader.SetMaxTransformedBytes(gw.Config.Governance.EffectiveMaxTransformedSSEBytes())
	if gw.Config.Governance.FilterPrivateSSEEvents {
		reader.SetDropPrivateEvents(true)
	}
	reader.SetLogger(gw.Logger)
	p.setupStreamFilterIfEnabled(gw, reader)
	p.setupStreamEntropyFilterIfEnabled(gw, reader)
	if guard := exfilGuardFor(gw, agentName); guard != nil {
		reader.SetExfilGuard(guard)
	}
	if canary.Token != "" {
		// Channel "stream": the same label the chat text path uses — the
		// guarded channel is model text, whichever endpoint carried it.
		reader.SetCanaryWatcher(stream.NewCanaryWatcher(canary.Token, canary.Action, "stream", gw.Metrics, gw.Logger.With("agent", agentName)))
	}
	// NENYA-344/345 wiring, verbatim: the assembler observes the raw
	// Responses events (feedResponses), the Phase 010 observer logs
	// completion activity, and the Phase 020 enforcing guards get dedicated
	// ExfilGuard + canary instances whose windows hold argument bytes.
	assembler := stream.NewToolCallAssembler(stream.DefaultToolArgsHoldBytes, toolArgsMaxBytes(gw), stream.DefaultToolArgsTotalBytes, stream.DefaultToolArgsMaxCalls)
	activity := &toolArgsLogObserver{
		logger:   gw.Logger.With("agent", agentName, "provider", providerName, "model", model, "endpoint", "responses"),
		fragLens: map[string]int{},
	}
	guards := toolArgsGuards(assembler, gw, transformReaderOpts{
		target:    routing.UpstreamTarget{Provider: providerName, Model: model},
		agentName: agentName,
		canary:    canary,
	})
	if guards != nil {
		assembler.SetObserver(&toolArgsFanOut{activity: activity, guards: guards})
	} else {
		assembler.SetObserver(activity)
	}
	reader.SetToolArgsAssembler(assembler)

	_, copyErr := copyStream(r.Context(), p.streamDestination(gw, w), reader, nil)
	if copyErr == nil {
		return
	}

	// Guard verdicts terminate the stream with the kind-specific Responses
	// error event; any other copy error ends the relay the way the raw
	// passthrough ends it (Debug log, no injected terminal).
	var kind string
	var message string
	switch {
	case errors.Is(copyErr, stream.ErrCanaryDetected):
		kind = string(infra.ErrorKindExfilDetected)
		message = "stream blocked: canary token detected in output"
	case errors.Is(copyErr, stream.ErrExfilBlocked):
		kind = string(infra.ErrorKindExfil)
		message = "stream blocked by data-exfiltration policy"
	case errors.Is(copyErr, stream.ErrStreamBlocked):
		// The secret/policy StreamFilter's verdict is an execution-policy
		// block, not data exfiltration — mirror the chat taxonomy
		// (writeBlockedSSE), which carries NO exfil kind: omit the code
		// fields entirely so clients keying on the ErrorKind closed set
		// never see an unregistered kind.
		kind = ""
		message = "stream blocked by execution policy"
	default:
		logger.DebugContext(r.Context(), "responses relay copy ended", "err", copyErr)
		return
	}
	if kind != "" {
		gw.Logger.Warn("responses stream blocked by output policy",
			"model", model, "provider", providerName, "code", kind)
	} else {
		gw.Logger.Warn("responses stream blocked by execution policy",
			"model", model, "provider", providerName)
	}
	gw.Metrics.RecordStreamBlock(model, providerName)
	writeResponsesBlockedEvent(gw, w, kind, message)
}

// streamDestination wraps the client writer for the guarded relay: the
// immediate-flush writer arms the NENYA-155 client-stall deadline before
// every write (a stuck reader must not pin the handler goroutine and the
// upstream connection forever — WriteTimeout is deliberately 0 for
// streaming) and flushes per write so SSE frames stream incrementally
// instead of bursting on net/http's ~4 KiB buffer. Falls back to the raw
// writer when it cannot flush.
func (p *Proxy) streamDestination(gw *gateway.NenyaGateway, w http.ResponseWriter) io.Writer {
	if fw, ok := newImmediateFlushWriter(w, resolveStreamStallTimeout(gw)); ok {
		return fw
	}
	return w
}

// writeResponsesBlockedEvent terminates a Responses stream with the
// canonical error event (event: error + a typed data payload), then flushes.
// kind is the wire code (and error_kind) — "" omits both, the shape the
// execution-policy class mirrors from the chat path (no unregistered kind).
// Mid-stream terminal (NENYA-155 contract): the client-stall deadline is
// re-armed first so the stale deadline left by the last streamed chunk
// cannot drop the frame.
func writeResponsesBlockedEvent(gw *gateway.NenyaGateway, w http.ResponseWriter, kind, message string) {
	ArmClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	payload := map[string]interface{}{
		"type":    "error",
		"message": message,
	}
	if kind != "" {
		payload["code"] = kind
		payload["error_kind"] = kind
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		payloadBytes = []byte(`{"type":"error","code":"internal_error","message":"stream blocked"}`)
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payloadBytes)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// relayResponsesBuffered relays a non-streaming 2xx body through the
// deterministic defenses: the JSON body's Responses output surfaces are
// scanned by the ExfilGuard URL policy (strip rewrites in place, block
// refuses the response) and the canary tripwire (block refuses, log
// removes the occurrence). The raw bytes are forwarded verbatim when no
// rewrite happened; a rewrite re-encodes the parsed body. A non-JSON body
// gets the raw-byte canary scan only (a substring check is shape-agnostic;
// the URL policy needs model text, not bytes that may be a diagnostic
// page).
func (p *Proxy) relayResponsesBuffered(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, resp *http.Response, providerName, model, agentName string, canary pipeline.CanarResult, logger *slog.Logger) {
	limit := maxResponsesBufferedBodyBytes(gw)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		logger.DebugContext(r.Context(), "responses buffered read ended", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	if int64(len(body)) > limit {
		writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "response body exceeds the scan buffer")
		return
	}

	var parsed map[string]interface{}
	isJSON := json.Unmarshal(body, &parsed) == nil && parsed != nil
	rewrote := false

	if isJSON {
		if guard := exfilGuardFor(gw, agentName); guard != nil {
			hits := inspectResponseTexts(guard, parsed)
			if hits.blocked {
				p.handleExfilBlock(gw, w, model, hits.reason)
				return
			}
			rewrote = hits.rewrote
		}
	}
	terminal, canaryRewrote, newBody := scanResponsesBufferedCanary(gw, w, canary, parsed, body, isJSON, logger)
	if terminal {
		return
	}
	if canaryRewrote {
		rewrote = true
		body = newBody
	}

	final := body
	if isJSON && rewrote {
		encoded, err := json.Marshal(parsed)
		if err != nil {
			// The scanned map is the only trustworthy copy now: fail closed
			// rather than forward a body whose rewrites were lost.
			gw.Logger.Error("scanned responses body failed to re-marshal; refusing the response", "err", err)
			writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "response could not be relayed after scanning")
			return
		}
		final = encoded
	}

	routing.CopyHeaders(resp.Header, w.Header())
	w.Header().Set("Content-Length", fmt.Sprint(len(final)))
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(final); err != nil {
		logger.DebugContext(r.Context(), "responses buffered write ended", "err", err)
	}
}

// scanResponsesBufferedCanary runs the canary tripwire over a buffered
// response body: the parsed JSON's text surfaces (inspectCanaryTexts strips
// occurrences in place under log action) or, for a non-JSON body, the raw
// bytes (a shape-agnostic substring splice that preserves the rest).
// Returns whether the response was refused (block action), whether anything
// was rewritten (with the new body when it was), and records the detection.
func scanResponsesBufferedCanary(gw *gateway.NenyaGateway, w http.ResponseWriter, canary pipeline.CanarResult, parsed map[string]interface{}, body []byte, isJSON bool, logger *slog.Logger) (blocked, rewrote bool, newBody []byte) {
	if canary.Token == "" {
		return false, false, body
	}
	hit := false
	if isJSON {
		hit = inspectCanaryTexts(parsed, canary.Token).hit
	} else {
		hit = pipeline.CanaryTripped(canary.Token, string(body))
	}
	if !hit {
		return false, false, body
	}
	gw.Metrics.RecordExfilEvent("buffered")
	logger.Warn("canary detected in buffered responses output",
		"channel", "buffered", "action", canary.Action)
	if canary.Action == config.CanaryActionBlock {
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindExfilDetected,
			"response blocked: canary token detected in output")
		return true, true, body
	}
	if isJSON {
		// inspectCanaryTexts already stripped the occurrences in place.
		return false, true, body
	}
	return false, true, []byte(strings.ReplaceAll(string(body), canary.Token, ""))
}
