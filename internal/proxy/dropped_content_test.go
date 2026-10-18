package proxy

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/gateway"
)

// NENYA-176: every unsendable-content report must surface to the client
// (X-Nenya-Dropped-Content header) and never silently disappear, including
// when several translations of the same request report different kinds.
func TestReportUnsendableContentHeadersMerge(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	p := &Proxy{}

	report := adapter.NewDropReport()
	report.Add("document")
	report.Add("thinking_signature")
	p.reportUnsendableContent(gw, w, r, report)

	if got := w.Header().Get("X-Nenya-Dropped-Content"); got != "document,thinking_signature" {
		t.Fatalf("header = %q, want document,thinking_signature", got)
	}

	// A second translation of the same request adds its kinds without losing
	// the first report's.
	second := adapter.NewDropReport()
	second.Add("redacted_thinking")
	second.Add("document") // dedup
	p.reportUnsendableContent(gw, w, r, second)
	if got := w.Header().Get("X-Nenya-Dropped-Content"); got != "document,redacted_thinking,thinking_signature" {
		t.Fatalf("merged header = %q, want document,redacted_thinking,thinking_signature", got)
	}

	// An empty report must not set or clobber the header.
	empty := adapter.NewDropReport()
	p.reportUnsendableContent(gw, w, r, empty)
	if got := w.Header().Get("X-Nenya-Dropped-Content"); got != "document,redacted_thinking,thinking_signature" {
		t.Fatalf("empty report clobbered header: %q", got)
	}
}

func TestReportUnsendableContentNilAndEmpty(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	p := &Proxy{}
	p.reportUnsendableContent(gw, w, r, nil)
	p.reportUnsendableContent(gw, w, r, adapter.NewDropReport())
	if _, set := w.Header()["X-Nenya-Dropped-Content"]; set {
		t.Fatal("no drops must not set the header")
	}
}
