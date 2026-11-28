package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/audit"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/testutil"
)

// NENYA-362 pins: the boot wiring arms the redaction interceptor with
// the audit sink — redactions land in the hash-chained file as
// structure-only entries, the redacted text never reaches the file, and
// the writer survives a gateway reload (the sink is carried, not
// reopened).
func TestRedactionAuditEndToEndThroughChain(t *testing.T) {
	logger := testutil.NewTestLogger()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	cfg := testutil.MinimalConfig()
	on := true
	cfg.Bouncer.Enabled = &on
	cfg.Governance.RedactionAudit = &config.RedactionAuditConfig{Enabled: &on, Path: path}

	w, err := audit.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "nk-loadtest-0123456789abc67890"}, logger)
	gw.SecretPatterns = []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	gw.Metrics = infra.NewMetrics()
	gw.RedactionAudit = &redactionAuditSink{w: w, metrics: gw.Metrics, logger: logger}

	chain, _, err := buildInterceptorChain(gw, cfg, logger)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	secret := "AKIAIOSFODNN7EXAMPLE"
	res, err := chain.Execute(context.Background(), &pipeline.InterceptRequest{
		AgentName: "demo-agent",
		Payload:   map[string]any{"model": "demo-model"},
		Messages:  []map[string]any{{"role": "user", "content": "body key=" + secret}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_ = res

	var out bytes.Buffer
	gw.Metrics.WritePrometheus(&out)
	if !strings.Contains(out.String(), `nenya_redaction_audit_entries_total{outcome="written"} 1`) {
		t.Fatalf("audit metric missing: %s", out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(raw), `"agent":"demo-agent"`) || !strings.Contains(string(raw), `"count":1`) {
		t.Fatalf("audit entry missing structure: %q", string(raw))
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("REDACTED CONTENT REACHED THE AUDIT FILE")
	}
	rep := audit.VerifyFile(path)
	if !rep.OK || rep.Entries != 1 {
		t.Fatalf("chain: %+v", rep)
	}

	// Reload carry: a rebuilt gateway pointing at the same sink appends
	// (seq 2), never reopens.
	gw2 := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "nk-loadtest-0123456789abc67890"}, logger)
	gw2.SecretPatterns = gw.SecretPatterns
	gw2.Metrics = gw.Metrics
	gw2.RedactionAudit = gw.RedactionAudit
	chain2, _, err := buildInterceptorChain(gw2, cfg, logger)
	if err != nil {
		t.Fatalf("chain2: %v", err)
	}
	_ = chain2
	if err := w.Record("demo-agent", audit.SurfaceBody, 1); err != nil {
		t.Fatalf("record after rebuild: %v", err)
	}
	rep = audit.VerifyFile(path)
	if !rep.OK || rep.Entries != 2 {
		t.Fatalf("chain after rebuild: %+v", rep)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
