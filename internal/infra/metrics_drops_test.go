package infra

import "testing"

func TestRecordUnsendableContent(t *testing.T) {
	m := NewMetrics()
	m.RecordUnsendableContent("") // empty kind ignored
	m.RecordUnsendableContent("document")
	m.RecordUnsendableContent("document")
	m.RecordUnsendableContent("thinking_signature")
	e := getOrCreateEntry(&m.unsendableContent, map[string]string{"kind": "document"})
	if got := e.value.Load(); got != uint64(2) {
		t.Fatalf("document count = %v, want 2", got)
	}
	var nilMetrics *Metrics
	nilMetrics.RecordUnsendableContent("document") // nil-safe, must not panic
}

// TestRecordUnsendableContent_NENYA337KindsLabeled pins the dropKindsAllowlist
// extension (NENYA-337): the openai-responses converters' kinds must get
// their own metric labels — a reverted extension silently re-collapses them
// to "other" and the per-kind attribution is lost.
func TestRecordUnsendableContent_NENYA337KindsLabeled(t *testing.T) {
	m := NewMetrics()
	kinds := []string{
		"stop", "logprobs", "seed", "user", "response_format", "tool_choice",
		"content_part_text", "tool_name", "tool_call_id", "messages",
		"tool_call", "content_part", "output_item", "reasoning_history",
		"reasoning_summary", "image_url",
	}
	for _, k := range kinds {
		m.RecordUnsendableContent(k)
		e := getOrCreateEntry(&m.unsendableContent, map[string]string{"kind": k})
		if got := e.value.Load(); got != uint64(1) {
			t.Errorf("kind %q collapsed to a shared label (count %v, want its own entry at 1)", k, got)
		}
	}
	// A truly unknown provider-native kind still collapses to "other".
	m.RecordUnsendableContent("some_future_provider_shape")
	e := getOrCreateEntry(&m.unsendableContent, map[string]string{"kind": "other"})
	if got := e.value.Load(); got != uint64(1) {
		t.Fatalf("unknown kind count = %v, want 1 under other", got)
	}
}
