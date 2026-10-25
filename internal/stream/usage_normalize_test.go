package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestNormalizeUsageTotals pins the NENYA-167 normalization invariants: every
// usage dimension is counted exactly once, whichever wire shape carried it, so
// prompt tokens include the cache dimensions and completion tokens include the
// reasoning tokens.
func TestNormalizeUsageTotals(t *testing.T) {
	tests := []struct {
		name  string
		usage map[string]interface{}
		want  UsageTotals
	}{
		{
			name:  "openai standard",
			usage: map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 20.0, "total_tokens": 120.0},
			want:  UsageTotals{Prompt: 100, Completion: 20, Total: 120, TotalReported: true},
		},
		{
			name: "openai cache split is a subset of prompt",
			usage: map[string]interface{}{
				"prompt_tokens": 105.0, "completion_tokens": 20.0, "total_tokens": 120.0,
				"prompt_cache_hit_tokens": 60.0, "prompt_cache_miss_tokens": 40.0, "cache_creation_tokens": 5.0,
			},
			want: UsageTotals{Prompt: 105, Completion: 20, Total: 120, CacheHit: 60, CacheMiss: 40, CacheCreation: 5, TotalReported: true},
		},
		{
			name: "openai nested cached details derive the miss (AUD-28)",
			usage: map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
				"prompt_tokens_details": map[string]interface{}{"cached_tokens": 80.0},
			},
			// Hit-rate invariant: hit + miss + creation == prompt, so the
			// miss is the derived remainder (80 + 20 == 100), not 0.
			want: UsageTotals{Prompt: 100, Completion: 20, Total: 120, CacheHit: 80, CacheMiss: 20},
		},
		{
			name: "openai explicit miss is honored and consistent",
			usage: map[string]interface{}{
				"prompt_tokens": 105.0, "completion_tokens": 20.0,
				"prompt_cache_hit_tokens": 60.0,
			},
			want: UsageTotals{Prompt: 105, Completion: 20, Total: 125, CacheHit: 60, CacheMiss: 45},
		},
		{
			name: "openai without a decomposition reports no dimensions",
			usage: map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
			},
			want: UsageTotals{Prompt: 100, Completion: 20, Total: 120},
		},
		{
			name: "openai reasoning is a subset of completion",
			usage: map[string]interface{}{
				"prompt_tokens": 10.0, "completion_tokens": 20.0, "total_tokens": 30.0,
				"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 12.0},
			},
			want: UsageTotals{Prompt: 10, Completion: 20, Total: 30, Reasoning: 12, TotalReported: true},
		},
		{
			name: "anthropic native input excludes the cache dimensions",
			usage: map[string]interface{}{
				"input_tokens": 30.0, "output_tokens": 10.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			},
			want: UsageTotals{
				Prompt: 100, Completion: 10, Total: 110,
				CacheHit: 60, CacheMiss: 30, CacheCreation: 10,
			},
		},
		{
			name:  "anthropic native without cache",
			usage: map[string]interface{}{"input_tokens": 30.0, "output_tokens": 10.0},
			want:  UsageTotals{Prompt: 30, Completion: 10, Total: 40, CacheMiss: 30},
		},
		{
			name: "gemini native thoughts are added to completion",
			usage: map[string]interface{}{
				"promptTokenCount": 25.0, "candidatesTokenCount": 40.0, "thoughtsTokenCount": 15.0,
				"totalTokenCount": 80.0, "cachedContentTokenCount": 5.0,
			},
			want: UsageTotals{Prompt: 25, Completion: 55, Total: 80, CacheHit: 5, CacheMiss: 20, Reasoning: 15, TotalReported: true},
		},
		{
			// A gateway in front of Anthropic that passes the Anthropic input
			// semantics (input EXCLUDES the cache dimensions) through under the
			// OpenAI key name, with no OpenAI-style cache decomposition: the
			// cache dimensions must be folded into the input total, not treated
			// as a subset of a total that never included them.
			name: "anthropic input semantics under the openai key name",
			usage: map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			},
			want: UsageTotals{
				Prompt: 170, Completion: 20, Total: 190,
				CacheHit: 60, CacheMiss: 100, CacheCreation: 10,
				AmbiguousCacheSemantics: true,
			},
		},
		{
			// The OpenAI decomposition proves the input total already includes
			// the cache dimensions, so the native fields are redundant and must
			// not be folded in (or the cache tokens would be counted twice).
			name: "openai decomposition wins over redundant native cache fields",
			usage: map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
				"prompt_cache_hit_tokens": 60.0, "cache_creation_tokens": 10.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			},
			want: UsageTotals{
				Prompt: 100, Completion: 20, Total: 120,
				CacheHit: 60, CacheMiss: 30, CacheCreation: 10,
			},
		},
		{
			name:  "string-typed numbers",
			usage: map[string]interface{}{"prompt_tokens": "7", "completion_tokens": "3"},
			want:  UsageTotals{Prompt: 7, Completion: 3, Total: 10},
		},
		{
			name:  "empty",
			usage: map[string]interface{}{},
			want:  UsageTotals{},
		},
		{
			name:  "nil",
			usage: nil,
			want:  UsageTotals{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeUsageTotals(tc.usage)
			if got != tc.want {
				t.Fatalf("NormalizeUsageTotals = %+v, want %+v", got, tc.want)
			}
			// The invariants themselves, asserted independently of the table.
			if got.CacheHit > got.Prompt || got.CacheCreation > got.Prompt {
				t.Errorf("cache dimensions must be a subset of prompt: %+v", got)
			}
			if got.Reasoning > got.Completion {
				t.Errorf("reasoning must be a subset of completion: %+v", got)
			}
			if got.CacheMiss != 0 && got.CacheHit+got.CacheMiss+got.CacheCreation != got.Prompt {
				t.Errorf("cache counters must sum to the input total: %+v", got)
			}
		})
	}
}

// TestUsageMapFromChunk pins the accepted placements, including the ones that
// are not the OpenAI standard (Anthropic's nested message usage, Gemini's
// usageMetadata, and a usage-only chunk with no choices).
func TestUsageMapFromChunk(t *testing.T) {
	tests := []struct {
		name  string
		chunk map[string]interface{}
		want  bool
	}{
		{"top-level usage", map[string]interface{}{"usage": map[string]interface{}{"prompt_tokens": 1.0}}, true},
		{"usage-only chunk", map[string]interface{}{"choices": []interface{}{}, "usage": map[string]interface{}{"total_tokens": 5.0}}, true},
		{"anthropic message_start", map[string]interface{}{
			"type":    "message_start",
			"message": map[string]interface{}{"usage": map[string]interface{}{"input_tokens": 3.0}},
		}, true},
		{"gemini usageMetadata", map[string]interface{}{"usageMetadata": map[string]interface{}{"promptTokenCount": 3.0}}, true},
		{"no usage", map[string]interface{}{"choices": []interface{}{}}, false},
		{"null usage", map[string]interface{}{"usage": nil}, false},
		{"non-map usage", map[string]interface{}{"usage": "nope"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := UsageMapFromChunk(tc.chunk); got != tc.want {
				t.Fatalf("UsageMapFromChunk ok = %v, want %v", got, tc.want)
			}
		})
	}
}

// collectUsage runs an SSE stream through the reader and returns the per-event
// usage deltas.
func collectUsage(t *testing.T, sse string) []UsageData {
	t.Helper()
	var events []UsageData
	reader := NewSSETransformingReader(strings.NewReader(sse), nil, context.Background())
	reader.SetOnUsage(func(u UsageData) { events = append(events, u) })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("unexpected reader error: %v", err)
	}
	return events
}

func sumUsage(events []UsageData) UsageData {
	var total UsageData
	for _, u := range events {
		total.CompletionTokens += u.CompletionTokens
		total.PromptTokens += u.PromptTokens
		total.TotalTokens += u.TotalTokens
		total.CacheHitTokens += u.CacheHitTokens
		total.CacheMissTokens += u.CacheMissTokens
		total.CacheCreationTokens += u.CacheCreationTokens
		total.ReasoningTokens += u.ReasoningTokens
	}
	return total
}

// TestReaderUsageAnthropicNativeStream pins that an Anthropic-native stream
// (which the reader sees before the response transformer runs) records usage:
// the input usage is nested under message_start.message and the output usage
// arrives on message_delta, and the cache dimensions must not be counted twice
// even when the provider repeats them.
func TestReaderUsageAnthropicNativeStream(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.PromptTokens != 100 {
		t.Errorf("prompt tokens = %d, want 100 (input 30 + cache read 60 + cache write 10)", got.PromptTokens)
	}
	if got.CompletionTokens != 12 {
		t.Errorf("completion tokens = %d, want 12", got.CompletionTokens)
	}
	if got.CacheHitTokens != 60 {
		t.Errorf("cache hit tokens = %d, want 60", got.CacheHitTokens)
	}
	if got.CacheCreationTokens != 10 {
		t.Errorf("cache creation tokens = %d, want 10", got.CacheCreationTokens)
	}
	if got.PromptTokens < got.CacheHitTokens+got.CacheCreationTokens {
		t.Errorf("cache dimensions must fit inside the prompt total: %+v", got)
	}
}

// TestReaderUsageRepeatedCacheFieldsCountedOnce pins that a provider repeating
// the cache fields in every chunk cannot inflate them: the reader tracks the
// native cache values as cumulative counters too.
func TestReaderUsageRepeatedCacheFieldsCountedOnce(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":10}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":10,"output_tokens":5}}` + "\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.PromptTokens != 100 {
		t.Errorf("prompt tokens = %d, want 100 (repeated cache fields counted once)", got.PromptTokens)
	}
	if got.CacheHitTokens != 60 || got.CacheCreationTokens != 10 {
		t.Errorf("cache tokens = %d/%d, want 60/10", got.CacheHitTokens, got.CacheCreationTokens)
	}
}

// TestReaderUsageGeminiNativeMetadata pins the Gemini-native placement
// (usageMetadata), where the candidate count excludes the thought tokens: the
// thoughts must land inside the completion total.
func TestReaderUsageGeminiNativeMetadata(t *testing.T) {
	sse := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":25,"candidatesTokenCount":40,"thoughtsTokenCount":15,"totalTokenCount":80,"cachedContentTokenCount":5}}` + "\n\n" +
		"data: [DONE]\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.PromptTokens != 25 {
		t.Errorf("prompt tokens = %d, want 25", got.PromptTokens)
	}
	if got.CompletionTokens != 55 {
		t.Errorf("completion tokens = %d, want 55 (40 candidates + 15 thoughts)", got.CompletionTokens)
	}
	if got.ReasoningTokens != 15 {
		t.Errorf("reasoning tokens = %d, want 15", got.ReasoningTokens)
	}
	if got.CacheHitTokens != 5 {
		t.Errorf("cache hit tokens = %d, want 5", got.CacheHitTokens)
	}
}

// TestReaderUsageOnlyFinalChunk pins the Together-class placement: a chunk with
// no choices that carries only usage must still be read (the OpenAI standard
// final usage chunk has the same shape).
func TestReaderUsageOnlyFinalChunk(t *testing.T) {
	sse := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n" +
		"data: [DONE]\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.PromptTokens != 11 || got.CompletionTokens != 7 {
		t.Fatalf("usage-only final chunk not read: %+v", got)
	}
}

// TestReaderUsageHighWaterCountsOnce pins the cumulative-accounting rule
// (NENYA-167): a provider that omits a field in one chunk and re-reports it
// later, or reports a smaller value after a partial one, must not have those
// tokens counted twice.
func TestReaderUsageHighWaterCountsOnce(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":30,"cache_read_input_tokens":60}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":30,"cache_read_input_tokens":5}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":10,"output_tokens":12}}` + "\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.PromptTokens != 100 {
		t.Errorf("prompt tokens = %d, want 100 (30 input + 60 read + 10 written, counted once)", got.PromptTokens)
	}
	if got.CacheHitTokens != 60 {
		t.Errorf("cache hit tokens = %d, want 60", got.CacheHitTokens)
	}
	if got.CacheCreationTokens != 10 {
		t.Errorf("cache creation tokens = %d, want 10", got.CacheCreationTokens)
	}
	if got.CompletionTokens != 12 {
		t.Errorf("completion tokens = %d, want 12", got.CompletionTokens)
	}
	// The derived total follows the parts: the input side arrived on the first
	// chunk and the output side on the last.
	if got.TotalTokens != 112 {
		t.Errorf("total tokens = %d, want 112", got.TotalTokens)
	}
}

// TestReaderUsageReportedTotalIsUsedAsIs pins that a provider-reported total is
// accounted as reported (the derived-total path is only for shapes whose total
// Nenya computes).
func TestReaderUsageReportedTotalIsUsedAsIs(t *testing.T) {
	sse := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":42}}` + "\n\n" +
		"data: [DONE]\n\n"

	got := sumUsage(collectUsage(t, sse))
	if got.TotalTokens != 42 {
		t.Errorf("total tokens = %d, want the reported 42", got.TotalTokens)
	}
}

// TestReaderUsageAmbiguousSemanticsSurfaces pins AUD-27 (NENYA-242): the
// streaming usage callback must carry AmbiguousCacheSemantics when the
// source object has an OpenAI-style input total with the Anthropic-native
// cache field names and no decomposition. Pre-fix the normalizer computed
// the flag but the reader never copied it into UsageData, so the streaming
// consumer's fold-note was dead code (only the buffered path surfaced it).
func TestReaderUsageAmbiguousSemanticsSurfaces(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":12,\"cache_read_input_tokens\":60,\"cache_creation_input_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"

	events := collectUsage(t, sse)
	if len(events) == 0 {
		t.Fatal("expected at least one usage event")
	}
	anyAmbiguous := false
	for _, u := range events {
		if u.AmbiguousCacheSemantics {
			anyAmbiguous = true
		}
	}
	if !anyAmbiguous {
		t.Fatalf("an OpenAI-input-total + native-cache-fields usage must surface AmbiguousCacheSemantics on the streaming callback")
	}
}

// TestReaderUsagePlainOpenAINotAmbiguous is the control: a plain OpenAI
// decomposition (or no cache fields at all) is unambiguous.
func TestReaderUsagePlainOpenAINotAmbiguous(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":60}}}\n\n" +
		"data: [DONE]\n\n"

	events := collectUsage(t, sse)
	for _, u := range events {
		if u.AmbiguousCacheSemantics {
			t.Fatalf("a decomposed OpenAI usage must not be flagged ambiguous: %+v", u)
		}
	}
}
