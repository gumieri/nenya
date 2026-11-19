package routing

// Transform-pipeline benchmarks over the fixture corpus (5a8943f0): one
// OpenCode-shaped payload dispatched through TransformRequestForUpstream to
// each target format — the per-dispatch cost every request pays on top of
// the content pipeline. Baseline for future hot-path work.

import (
	"encoding/json"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/ssefixtures"
)

func benchTransformDeps(b *testing.B) TransformDeps {
	b.Helper()
	cfg := &config.Config{}
	if err := config.ApplyDefaults(cfg); err != nil {
		b.Fatalf("ApplyDefaults: %v", err)
	}
	providers := config.ResolveProviders(cfg, &config.SecretsConfig{
		ProviderKeys: map[string]string{"openai": "bench-openai-key", "anthropic": "bench-anthropic-key"},
	})
	return TransformDeps{
		Config:    cfg,
		Providers: providers,
		Metrics:   infra.NewMetrics(),
	}
}

// benchTransformPayload parses the fixture: the same parsed map is reused
// across iterations (production keeps the parsed client payload across
// failover attempts; the transform restores the shared map's model ID and
// renamed token caps after each dispatch, NENYA-179 — canonical-field
// insertions like max_tokens persist by design, NENYA-186; reuse is
// idempotent: identical marshal output from iteration 2 onward) — this
// bench measures the per-dispatch transform, not the per-request parse.
func benchTransformPayload(b *testing.B) map[string]interface{} {
	b.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(ssefixtures.ChatPayloadJSON(21, 400), &payload); err != nil {
		b.Fatalf("fixture payload: %v", err)
	}
	return payload
}

func benchmarkTransformForUpstream(b *testing.B, format, model, provider string) {
	deps := benchTransformDeps(b)
	params := TransformParams{ProviderName: provider, Model: model, Format: format}
	payload := benchTransformPayload(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, _, err := TransformRequestForUpstream(deps, params, payload)
		if err != nil {
			b.Fatalf("transform: %v", err)
		}
		if len(body) == 0 {
			b.Fatal("empty transformed body")
		}
	}
}

func BenchmarkTransformRequestForUpstream_OpenAI(b *testing.B) {
	benchmarkTransformForUpstream(b, "openai", "gpt-6-luna", "openai")
}

func BenchmarkTransformRequestForUpstream_OpenAIResponses(b *testing.B) {
	benchmarkTransformForUpstream(b, "openai-responses", "gpt-6-luna", "openai")
}

func BenchmarkTransformRequestForUpstream_Anthropic(b *testing.B) {
	benchmarkTransformForUpstream(b, "anthropic", "claude-bench", "anthropic")
}
