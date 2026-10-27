package infra

import (
	"strings"
	"testing"
)

// TestRecordClientStalledGuardsModelLabel pins AUD-66 (NENYA-289): the
// stalled/stream-stall counters carry the model label through the SAME
// cardinality guard as every other model-labeled counter — a hostile model
// id (invalid or beyond the series budget) must collapse, not create
// unbounded series. Pre-fix RecordClientStalled/RecordStreamStall wrote the
// raw model string straight into the label map.
func TestRecordClientStalledGuardsModelLabel(t *testing.T) {
	m := NewMetrics()
	hostile := strings.Repeat("x", 100) // >64 bytes: implausible id

	m.RecordClientStalled(hostile, "prov")
	m.RecordStreamStall(hostile, "prov")

	var sb strings.Builder
	m.WritePrometheus(&sb)
	if strings.Contains(sb.String(), hostile) {
		t.Fatalf("the hostile model id must be guarded out of the labels:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "nenya_client_stalled_total") ||
		!strings.Contains(sb.String(), "nenya_stream_stalled_total") {
		t.Fatalf("both counters must fire:\n%s", sb.String())
	}
}

// TestRecordClientStalledUnknownModel pins the empty-model normalization:
// the MCP/passthrough callers pass "" for a model they cannot attribute —
// the guard maps it to the shared unknown label.
func TestRecordClientStalledUnknownModel(t *testing.T) {
	m := NewMetrics()
	m.RecordClientStalled("", "prov")

	var sb strings.Builder
	m.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `model="unknown"`) {
		t.Fatalf("the empty model must normalize to the unknown label:\n%s", sb.String())
	}
	if strings.Contains(sb.String(), `model=""`) {
		t.Fatalf("no empty model label may remain:\n%s", sb.String())
	}
}
