package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

// NENYA-346 (Module A Phase 030): the /v1/responses passthrough gains the
// output-side security pipeline — egress guards and the streamed tool-call
// argument assembler with its enforcing guards — default-ON behind
// governance.responses_pipeline.enabled. These tests pin the endpoint-level
// behavior the passthrough previously bypassed entirely.

// responsesFallbackCanary is what a test upstream echoes when the forwarded
// request carries NO injected canary marker (pre-injection builds). Post-fix
// the gateway injects a real token, so the echo is the injected one and the
// watcher matches it.
const responsesFallbackCanary = "NENYA-CANARY-0123456789abcdef0123456789abcdef"

var responsesCanaryRe = regexp.MustCompile(`NENYA-CANARY-[0-9a-f]{32}`)

// upstreamCanaryFromBody extracts the injected canary from the forwarded
// request body (the model reproducing injected context), falling back to a
// fixed token so pre-pipeline builds still echo a canary-shaped marker.
func upstreamCanaryFromBody(body []byte) string {
	if m := responsesCanaryRe.Find(body); m != nil {
		return string(m)
	}
	return responsesFallbackCanary
}

// responsesTestProxy builds a Proxy whose sole provider serves the
// /v1/responses surface from upstreamURL (BaseURL ending in /v1, the shape
// resolveResponsesURL requires).
func responsesTestProxy(t *testing.T, upstreamURL string, mutate func(*config.Config)) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			// deriveBaseURL strips the path, so the Responses route targets
			// upstreamURL + /v1/responses (the shape resolveResponsesURL
			// builds from a /v1-suffixed base). The bearer style + key make
			// getDefaultResponseProvider deterministically pick this
			// provider (the body's model names the agent, not a catalog
			// entry).
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "bearer",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
		ProviderKeys: map[string]string{
			"test-provider": "test-api-key",
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// responsesFrame renders one SSE frame of a Responses stream.
func responsesFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// canaryEnabled enables the canary tripwire in block mode.
func canaryEnabled(cfg *config.Config) {
	cfg.Governance.Canary = &config.CanaryConfig{
		Enabled: config.PtrTo(true),
		Action:  config.CanaryActionBlock,
	}
}

// exfilStripEnabled turns the egress guard to strip action.
func exfilStripEnabled(cfg *config.Config) {
	cfg.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: config.PtrTo(true),
		Action:  config.ExfilActionStrip,
	}
}

// privateMetaURL is a link-local exfiltration destination every guard
// configuration treats as a violation.
const privateMetaURL = "http://169.254.169.254/latest/meta-data/"

// responsesStripPlaceholder mirrors stream.responsesStripPlaceholder (unexported
// there): the marker spliced in place of a stripped URL.
const responsesStripPlaceholder = "[NENYA: EXFIL LINK REMOVED]"

// TestResponsesPipeline_InjectsCanaryIntoInstructions pins the request-side
// half of the pipeline: a POST /v1/responses create request dispatched
// upstream carries the canary system-context marker in the instructions
// channel, so the response-path watcher watches a token the upstream
// actually saw.
func TestResponsesPipeline_InjectsCanaryIntoInstructions(t *testing.T) {
	var forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		n, _ := r.Body.Read(buf)
		forwarded = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, canaryEnabled)
	body := `{"model":"test-agent","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(forwarded, "[internal-ref] NENYA-CANARY-") {
		t.Fatalf("forwarded request carries no canary marker (instructions channel):\n%s", forwarded)
	}
	if !strings.Contains(forwarded, `"instructions"`) {
		t.Fatalf("forwarded request carries no instructions field:\n%s", forwarded)
	}
}

// TestResponsesPipeline_TextDeltaCanaryBlocked pins the streaming text-path
// enforcement: a canary token inside a response.output_text.delta is caught
// by the inline canary watcher BEFORE the frame is forwarded — the client
// never receives the token, the carrying frame is suppressed, and the stream
// terminates with a structured error event instead of response.completed.
func TestResponsesPipeline_TextDeltaCanaryBlocked(t *testing.T) {
	var canary string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		n, _ := r.Body.Read(buf)
		canary = upstreamCanaryFromBody(buf[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responsesFrame("response.created",
			`{"type":"response.created","response":{"id":"resp_1"}}`))
		_, _ = fmt.Fprint(w, responsesFrame("response.output_text.delta",
			fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"context dump %s"}`, canary)))
		_, _ = fmt.Fprint(w, responsesFrame("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`))
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, canaryEnabled)
	body := `{"model":"test-agent","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream already committed)", rec.Code)
	}
	out := rec.Body.String()
	if strings.Contains(out, "NENYA-CANARY-") {
		t.Fatalf("canary token leaked to the client:\n%s", out)
	}
	if !strings.Contains(out, `"error_kind":"exfil_detected"`) {
		t.Fatalf("no canary block terminal in the stream:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("response.completed forwarded past a block verdict:\n%s", out)
	}
}

// TestResponsesPipeline_TextDeltaURLStripped pins the streaming egress URL
// policy: a private link-local URL inside a text delta is spliced to the
// placeholder before the frame is forwarded.
func TestResponsesPipeline_TextDeltaURLStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responsesFrame("response.output_text.delta",
			fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"see %s now"}`, privateMetaURL)))
		_, _ = fmt.Fprint(w, responsesFrame("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`))
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, exfilStripEnabled)
	body := `{"model":"test-agent","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "169.254.169.254") {
		t.Fatalf("private URL forwarded unstripped in a text delta:\n%s", out)
	}
	if !strings.Contains(out, responsesStripPlaceholder) {
		t.Fatalf("placeholder missing from the stripped delta:\n%s", out)
	}
	// The NENYA-205 hold withholds the whitespace-free tail ("now") until
	// the terminal drain, so the space that separated it from the URL sits
	// at a frame boundary — assert the words, not the seam.
	if !strings.Contains(out, "see ") || !strings.Contains(out, "now") {
		t.Fatalf("surrounding text lost by the strip:\n%s", out)
	}
}

// TestResponsesPipeline_ArgsDeltaCanaryStopLoss pins the streamed
// tool-argument channel semantics on this endpoint (the A020 design): the
// assembler's enforcing guard detects the token with at most the hold-back
// delay, the detection chunk and the terminal are suppressed, and the stream
// ends with the structured error — bytes already forwarded in earlier frames
// cannot be recalled (stop-loss, not retroactive redaction).
func TestResponsesPipeline_ArgsDeltaCanaryStopLoss(t *testing.T) {
	const tailSentinel = "tail-secret-payload-xyz"
	var canary string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		n, _ := r.Body.Read(buf)
		canary = upstreamCanaryFromBody(buf[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responsesFrame("response.output_item.added",
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","name":"get_weather","arguments":""}}`))
		// First delta: padding followed by the full canary token.
		_, _ = fmt.Fprint(w, responsesFrame("response.function_call_arguments.delta",
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"%s%s"}`,
				strings.Repeat("a", 120), canary)))
		// Second delta: enough bytes to push the token out of the assembler's
		// held tail — the guard detects here and this frame is suppressed.
		_, _ = fmt.Fprint(w, responsesFrame("response.function_call_arguments.delta",
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"%s"}`, tailSentinel+strings.Repeat("b", 120))))
		_, _ = fmt.Fprint(w, responsesFrame("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`))
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, canaryEnabled)
	body := `{"model":"test-agent","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream already committed)", rec.Code)
	}
	out := rec.Body.String()
	if strings.Contains(out, tailSentinel) {
		t.Fatalf("post-detection chunk forwarded past the block verdict:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("response.completed forwarded past a block verdict:\n%s", out)
	}
	if !strings.Contains(out, `"error_kind":"exfil_detected"`) {
		t.Fatalf("no canary block terminal in the stream:\n%s", out)
	}
}

// TestResponsesPipeline_ArgsDeltaExemptFromTextRedaction pins the channel
// boundary: a function_call_arguments delta is TOOL ARGUMENTS, owned by the
// assembler and its guards — the text-surface redaction must not splice it
// (a mid-argument rewrite would corrupt the reassembled JSON). With strip
// action the argument channel records the detection and lets the bytes
// through (A020 strip semantics: forwarded bytes cannot be rewritten; block
// is the keeping action).
func TestResponsesPipeline_ArgsDeltaExemptFromTextRedaction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responsesFrame("response.function_call_arguments.delta",
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\\"url\\":\\"%s\\"}"}`, privateMetaURL)))
		_, _ = fmt.Fprint(w, responsesFrame("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`))
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, exfilStripEnabled)
	body := `{"model":"test-agent","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "169.254.169.254") {
		t.Fatalf("argument delta was redacted by the TEXT surface path (corrupts reassembled JSON):\n%s", out)
	}
}

// TestResponsesPipeline_BufferedURLStripped pins the buffered relay: a
// non-streaming JSON response has its text surfaces (Responses output items)
// scanned and stripped, with the body re-encoded only when a rewrite
// happened.
func TestResponsesPipeline_BufferedURLStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"see %s now"}]}]}`,
			privateMetaURL)
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, exfilStripEnabled)
	body := `{"model":"test-agent","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "169.254.169.254") {
		t.Fatalf("private URL forwarded unstripped in a buffered response:\n%s", out)
	}
	if !strings.Contains(out, responsesStripPlaceholder) {
		t.Fatalf("placeholder missing from the buffered body:\n%s", out)
	}
	// The re-encoded body must stay a valid Responses object.
	if !strings.Contains(out, `"object":"response"`) || !strings.Contains(out, `"output"`) {
		t.Fatalf("buffered rewrite lost the response structure:\n%s", out)
	}
}

// TestResponsesPipeline_BufferedCanaryBlocked pins the buffered canary
// terminal: the echoed token inside a function_call item's arguments fails
// the request with a structured 403 and the body is withheld entirely.
func TestResponsesPipeline_BufferedCanaryBlocked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		n, _ := r.Body.Read(buf)
		canary := upstreamCanaryFromBody(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","id":"fc_1","name":"exfil","arguments":"{\"q\":\"%s\"}"}]}`,
			canary)
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, canaryEnabled)
	body := `{"model":"test-agent","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "NENYA-CANARY-") {
		t.Fatalf("canary-bearing body forwarded to the client:\n%s", out)
	}
	if !strings.Contains(out, string(config.CanaryActionBlock)) && !strings.Contains(out, "exfil_detected") {
		t.Fatalf("structured error missing the detection kind:\n%s", out)
	}
}

// TestResponsesPipeline_KillSwitchPassthrough pins the kill switch: with
// responses_pipeline.enabled=false the endpoint is the raw passthrough again
// — guards bypassed, bytes verbatim.
func TestResponsesPipeline_KillSwitchPassthrough(t *testing.T) {
	rawBody := fmt.Sprintf(
		`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"see %s now"}]},{"type":"function_call","id":"fc_1","name":"exfil","arguments":"{\"q\":\"%s\"}"}]}`,
		privateMetaURL, responsesFallbackCanary)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, rawBody)
	}))
	defer upstream.Close()

	p := responsesTestProxy(t, upstream.URL, func(cfg *config.Config) {
		canaryEnabled(cfg)
		exfilStripEnabled(cfg)
		cfg.Governance.ResponsesPipeline = &config.ResponsesPipelineConfig{Enabled: config.PtrTo(false)}
	})
	body := `{"model":"test-agent","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/responses", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if out != rawBody {
		t.Fatalf("kill-switch relay modified the body:\n got: %s\nwant: %s", out, rawBody)
	}
}
