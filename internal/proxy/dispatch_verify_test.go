package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/audit"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
)

// NENYA-363 pins: the dispatch verification pass over the final
// upstream body. Log leaves bytes identical; redact splices the label
// keeping JSON valid; block refuses with errDispatchBlocked; the
// matched TEXT never reaches metrics, logs, or the audit sink.

type recordingDispatchSink struct {
	agent   string
	surface string
	count   int
	calls   int
}

func (s *recordingDispatchSink) RecordRedactionEvent(agent string, surface string, count int) {
	s.calls++
	s.agent, s.surface, s.count = agent, surface, count
}

func dispatchVerifyGW(t *testing.T, action string, sink pipeline.RedactionAuditSink, patterns ...string) *gateway.NenyaGateway {
	t.Helper()
	on := true
	cfg := config.Config{}
	cfg.Governance.DispatchVerify = &config.DispatchVerifyConfig{Enabled: &on, Action: action}
	cfg.Bouncer.RedactionLabel = "[REDACTED]"
	gw := &gateway.NenyaGateway{
		Config:  cfg,
		Metrics: infra.NewMetrics(),
		Logger:  infra.SetupLogger(false),
		Providers: map[string]*config.Provider{
			"prov": {Name: "prov", URL: "https://up.test/v1/chat/completions", ApiFormat: "openai", AuthStyle: "none"},
		},
	}
	for _, src := range patterns {
		gw.SecretPatterns = append(gw.SecretPatterns, regexp.MustCompile(src))
	}
	gw.RedactionAudit = sink
	return gw
}

const dispatchSecret = "AKIAIOSFODNN7EXAMPLE"

func TestDispatchVerifyLogLeavesBodyIdentical(t *testing.T) {
	gw := dispatchVerifyGW(t, config.DispatchActionLog, nil, `AKIA[0-9A-Z]{16}`)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"key=` + dispatchSecret + `"}]}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "demo-agent", "demo-model", body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("log action mutated the body: %q", string(out))
	}
	var buf bytes.Buffer
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_dispatch_verify_total{action="log"} 1`) {
		t.Fatalf("log metric missing: %s", buf.String())
	}
}

func TestDispatchVerifyRedactSplicesLabel(t *testing.T) {
	sink := &recordingDispatchSink{}
	gw := dispatchVerifyGW(t, config.DispatchActionRedact, sink, `AKIA[0-9A-Z]{16}`)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"key=` + dispatchSecret + ` tail"}]}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "demo-agent", "demo-model", body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if bytes.Contains(out, []byte(dispatchSecret)) {
		t.Fatal("secret survived redact")
	}
	if !bytes.Contains(out, []byte(`key=[REDACTED] tail`)) {
		t.Fatalf("label splice wrong: %q", string(out))
	}
	if !json.Valid(out) {
		t.Fatalf("redact broke JSON validity: %q", string(out))
	}
	// The rest of the body is byte-identical outside the replaced span.
	want := `{"model":"m","messages":[{"role":"user","content":"key=[REDACTED] tail"}]}`
	if string(out) != want {
		t.Fatalf("body drift outside the span:\n got %q\nwant %q", string(out), want)
	}
	if sink.calls != 1 || sink.agent != "demo-agent" || sink.surface != "dispatch" || sink.count != 1 {
		t.Fatalf("audit entry wrong: %+v", sink)
	}
}

func TestDispatchVerifyBlockRefuses(t *testing.T) {
	gw := dispatchVerifyGW(t, config.DispatchActionBlock, nil, `AKIA[0-9A-Z]{16}`)
	body := []byte(`{"secret":"` + dispatchSecret + `"}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "", "m", body)
	if !errors.Is(err, errDispatchBlocked) {
		t.Fatalf("err = %v, want errDispatchBlocked", err)
	}
	if out != nil {
		t.Fatal("block returned a body")
	}
	var buf bytes.Buffer
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_dispatch_verify_total{action="block"} 1`) {
		t.Fatalf("block metric missing: %s", buf.String())
	}
}

func TestDispatchVerifyDisabledAndClean(t *testing.T) {
	off := false
	cfg := config.Config{}
	cfg.Governance.DispatchVerify = &config.DispatchVerifyConfig{Enabled: &off, Action: config.DispatchActionBlock}
	gw := &gateway.NenyaGateway{Config: cfg, Metrics: infra.NewMetrics(), Logger: infra.SetupLogger(false),
		SecretPatterns: []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}}
	body := []byte(`{"secret":"` + dispatchSecret + `"}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "", "m", body)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatalf("disabled gate interfered: %v", err)
	}
	// Enabled but no patterns: pass-through.
	gw2 := dispatchVerifyGW(t, config.DispatchActionBlock, nil)
	out2, err2 := (&Proxy{}).verifyDispatchBody(gw2, "", "m", body)
	if err2 != nil || !bytes.Equal(out2, body) {
		t.Fatalf("pattern-less gate interfered: %v", err2)
	}
	// Enabled, patterns, clean body: no metric, no error.
	clean := []byte(`{"hello":"world"}`)
	out3, err3 := (&Proxy{}).verifyDispatchBody(gw, "", "m", clean)
	if err3 != nil || !bytes.Equal(out3, clean) {
		t.Fatalf("clean body interfered: %v", err3)
	}
	var buf bytes.Buffer
	gw.Metrics.WritePrometheus(&buf)
	if strings.Contains(buf.String(), `nenya_dispatch_verify_total{action=`) {
		t.Fatalf("clean body fired the metric: %s", buf.String())
	}
}

func TestDispatchVerifyMatchCollection(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`[0-9A-Z]{16}`), // overlaps the first match's span
	}
	body := []byte(`x AKIAIOSFODNN7EXAMPLE y`)
	matches := collectDispatchMatches(patterns, body)
	if len(matches) != 1 {
		t.Fatalf("matches = %d (%+v), want 1 (overlap collapses to the first pattern)", len(matches), matches)
	}
	if matches[0].pattern != 0 {
		t.Fatalf("winning pattern = %d, want 0 (first pattern claims the span)", matches[0].pattern)
	}
	if got := string(body[matches[0].start:matches[0].end]); got != dispatchSecret {
		t.Fatalf("span text = %q", got)
	}
}

// TestDispatchVerifyStraddlerClipped pins the round-1 MED fix: a match
// that starts inside a kept span and extends past its end is CLIPPED to
// the unclaimed tail — redaction covers the tail too, so no
// pattern-matched bytes survive in the shipped body.
func TestDispatchVerifyStraddlerClipped(t *testing.T) {
	gw := dispatchVerifyGW(t, config.DispatchActionRedact, nil,
		`abcdef12`, // kept span [6,14)
		`12ghij`,   // straddler: starts inside, extends past the kept span's end
	)
	body := []byte(`{"v":"abcdef12ghij"}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "", "m", body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("redact broke JSON: %q", string(out))
	}
	want := []byte(`{"v":"[REDACTED][REDACTED]"}`)
	if !bytes.Equal(out, want) {
		t.Fatalf("clipped straddler wrong:\n got %q\nwant %q", string(out), string(want))
	}
}

func TestDispatchVerifyRedactQuoteSpanningFallsBackToLog(t *testing.T) {
	sink := &recordingDispatchSink{}
	gw := dispatchVerifyGW(t, config.DispatchActionRedact, sink, `pass":\s*"[^"]*`)
	// The pattern spans a JSON structural quote pair — splicing the label
	// would corrupt the document, so the pass falls back to log (body
	// unchanged, no audit entry).
	body := []byte(`{"note":"x","pass": "hunter2secret","z":1}`)
	out, err := (&Proxy{}).verifyDispatchBody(gw, "", "m", body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("fallback mutated the body: %q", string(out))
	}
	if sink.calls != 0 {
		t.Fatalf("fallback wrote an audit entry: %+v", sink)
	}
	var buf bytes.Buffer
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_dispatch_verify_total{action="log"} 1`) {
		t.Fatalf("fallback metric should be log with count 1: %s", buf.String())
	}
}

// TestDispatchVerifyPrecheckProbeHasNoSideEffects pins the verdict-only
// probe (round-2 MED): dispatchVerifyBlocked answers the block question
// without recording anything — redact/log mode never probes true, block
// mode probes true on matches only, and the metric series stays empty
// (the shipping body's pass records exactly once).
func TestDispatchVerifyPrecheckProbeHasNoSideEffects(t *testing.T) {
	body := []byte(`{"s":"` + dispatchSecret + `"}`)
	clean := []byte(`{"hello":"world"}`)

	redactGW := dispatchVerifyGW(t, config.DispatchActionRedact, nil, `AKIA[0-9A-Z]{16}`)
	if (&Proxy{}).dispatchVerifyBlocked(redactGW, body) {
		t.Fatal("redact mode probed true")
	}
	logGW := dispatchVerifyGW(t, config.DispatchActionLog, nil, `AKIA[0-9A-Z]{16}`)
	if (&Proxy{}).dispatchVerifyBlocked(logGW, body) {
		t.Fatal("log mode probed true")
	}
	blockGW := dispatchVerifyGW(t, config.DispatchActionBlock, nil, `AKIA[0-9A-Z]{16}`)
	if !(&Proxy{}).dispatchVerifyBlocked(blockGW, body) {
		t.Fatal("block mode with a match probed false")
	}
	if (&Proxy{}).dispatchVerifyBlocked(blockGW, clean) {
		t.Fatal("block mode on a clean body probed true")
	}
	// After four probes across three gateways: no metric recorded anywhere.
	for i, gw := range []*gateway.NenyaGateway{redactGW, logGW, blockGW} {
		var buf bytes.Buffer
		gw.Metrics.WritePrometheus(&buf)
		if strings.Contains(buf.String(), `nenya_dispatch_verify_total{action=`) {
			t.Fatalf("probe %d recorded a side effect: %s", i, buf.String())
		}
	}
}

func TestBuildUpstreamRequestVerifiesBody(t *testing.T) {
	gw := dispatchVerifyGW(t, config.DispatchActionRedact, nil, `AKIA[0-9A-Z]{16}`)
	prx := &Proxy{}
	src := http.Header{}
	req, verified, err := prx.buildUpstreamRequest(gw, context.Background(), http.MethodPost,
		"https://up.test/v1/chat/completions",
		[]byte(`{"m":"x","c":"key=`+dispatchSecret+`"}`), "prov", "model-1", "", "", src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !bytes.Contains(verified, []byte("[REDACTED]")) || bytes.Contains(verified, []byte(dispatchSecret)) {
		t.Fatalf("returned body not verified: %q", string(verified))
	}
	// The REQUEST carries the verified bytes (single read via GetBody).
	got, err := req.GetBody()
	if err != nil {
		t.Fatalf("getbody: %v", err)
	}
	decoded, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(decoded, verified) {
		t.Fatalf("request body != verified body:\n %q\n %q", string(decoded), string(verified))
	}

	// Block mode: buildUpstreamRequest refuses with the sentinel.
	gwb := dispatchVerifyGW(t, config.DispatchActionBlock, nil, `AKIA[0-9A-Z]{16}`)
	_, _, err = prx.buildUpstreamRequest(gwb, context.Background(), http.MethodPost,
		"https://up.test/v1/chat/completions",
		[]byte(`{"s":"`+dispatchSecret+`"}`), "prov", "model-1", "", "", src)
	if !errors.Is(err, errDispatchBlocked) {
		t.Fatalf("block err = %v, want errDispatchBlocked", err)
	}
}

func TestHandleResumeActionDispatchBlockedTerminal(t *testing.T) {
	gw := dispatchVerifyGW(t, config.DispatchActionLog, nil, `AKIA[0-9A-Z]{16}`)
	p := &Proxy{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	opts := forwardOptions{
		Targets:   []routing.UpstreamTarget{{Provider: "prov", Model: "m", CoolKey: "a|x|prov|m"}},
		AgentName: "a",
	}
	rl, err := newRetryLoop(p, gw, rec, r, opts)
	if err != nil {
		t.Fatalf("newRetryLoop: %v", err)
	}
	outcome := rl.handleResumeAction(upstreamAction{Kind: actionContinue, SkipReason: "dispatch_blocked"})
	if !outcome.handled {
		t.Fatal("dispatch block was not terminal (would failover)")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dispatch_blocked") {
		t.Fatalf("structured error missing the kind: %q", rec.Body.String())
	}
}

// The audit surface "dispatch" must be accepted by the audit writer and
// rejected surfaces stay rejected (closed-set pin across the D010/D020
// boundary).
func TestDispatchSurfaceAuditable(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	w, err := audit.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Record("model-1", audit.SurfaceDispatch, 2); err != nil {
		t.Fatalf("dispatch surface rejected: %v", err)
	}
	if err := w.Record("model-1", "sidechannel", 1); err == nil {
		t.Fatal("unknown surface accepted")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rep := audit.VerifyFile(path)
	if !rep.OK || rep.Entries != 1 {
		t.Fatalf("dispatch entry broke the chain: %+v", rep)
	}
}
