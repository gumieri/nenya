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
