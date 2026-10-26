package proxy

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// mcpUsageBuffer builds a bufferedSSE carrying one OpenAI usage chunk with a
// cache hit, attributed to the given serving target.
func mcpUsageBuffer(target routing.UpstreamTarget) *bufferedSSE {
	raw := "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":800}}}\n\n" +
		"data: [DONE]\n\n"
	buf := &bufferedSSE{
		rawBytes:         []byte(raw),
		model:            target.Model,
		servingTarget:    target,
		calibrationModel: target.Model,
	}
	return buf
}

// TestMCPIterationUsageRecordsCacheWarmth pins AUD-15 (NENYA-259): the MCP
// loop's per-iteration usage claiming must observe the pair's cache warmth
// (infra CacheStateTracker) exactly like the buffered chat path
// (recordNonStreamingUsage) and the streaming path (recordStreamUsage)
// — NENYA-190's warm-cache reorder tie-break is otherwise blind to every
// request an MCP-configured agent makes. Pre-fix the MCP site recorded
// stats, metrics, cost, and tracing but never CacheState.Record.
func TestMCPIterationUsageRecordsCacheWarmth(t *testing.T) {
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "t"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(gw.Close)

	target := routing.UpstreamTarget{Provider: "prov1", Model: "test-model", Format: "openai"}
	p := &Proxy{}
	p.StoreGateway(gw)

	buf := mcpUsageBuffer(target)
	p.recordMCPIterationUsage(context.Background(), gw, buf, []routing.UpstreamTarget{target}, "test-agent")

	snap := gw.CacheState.Snapshot()
	found := false
	for _, entry := range snap {
		if entry.Model == "test-model" && entry.Provider == "prov1" {
			found = true
			if entry.InputTokens == 0 {
				t.Fatal("warmth entry observed but the input total is zero")
			}
		}
	}
	if !found {
		t.Fatalf("the MCP usage site must observe cache warmth for (test-model, prov1); snapshot = %v", snap)
	}
}
