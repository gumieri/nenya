package routing

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/util"
)

// TestRuneSafeTruncate pins AUD-78 (NENYA-275): the unknown-field log cap
// must cut on a UTF-8 rune boundary. Pre-fix auditRequestFields sliced
// field[:64] raw, splitting multi-byte runes and logging invalid UTF-8
// fragments for non-ASCII field names (masked by slog's quoting at the
// rendered-log layer, but invalid at the value layer).
func TestRuneSafeTruncate(t *testing.T) {
	// 40 three-byte runes = 120 bytes: byte 64 lands mid-rune.
	long := strings.Repeat("日", 40)
	got := util.RuneSafeTruncate(long, 64)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated name is not valid UTF-8: %q", got)
	}
	if len(got) != 63 { // 21 complete 3-byte runes fit under the cap
		t.Fatalf("truncated length = %d, want 63 (the last full rune boundary)", len(got))
	}
	if !strings.HasPrefix(long, got) {
		t.Fatal("the truncation must be a prefix of the input")
	}

	// ASCII within the cap is unchanged.
	if got := util.RuneSafeTruncate("plain_field", 64); got != "plain_field" {
		t.Fatalf("short ASCII input = %q, want unchanged", got)
	}
	// ASCII over the cap cuts at the cap exactly.
	if got := util.RuneSafeTruncate(strings.Repeat("a", 100), 64); len(got) != 64 {
		t.Fatalf("ASCII cap length = %d, want 64", len(got))
	}
	// Degenerate caps.
	if got := util.RuneSafeTruncate("x", 0); got != "" {
		t.Fatalf("zero cap = %q, want empty", got)
	}
}

// captureHandler records the attr values of every handled record at the
// VALUE layer — slog's text quoting would mask invalid UTF-8 bytes.
type captureHandler struct {
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler            { return h }

// TestAuditUnknownFieldLogStaysValidUTF8 drives the audit end-to-end and
// asserts the logged fields attr is valid UTF-8 at the value layer.
func TestAuditUnknownFieldLogStaysValidUTF8(t *testing.T) {
	handler := &captureHandler{}
	deps := TransformDeps{
		Logger:  slog.New(handler),
		Config:  &config.Config{},
		Metrics: infra.NewMetrics(),
	}

	longField := strings.Repeat("日", 40)
	payload := map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{},
		longField:  true,
	}

	auditRequestFields(deps, payload, "openai")

	var fields string
	for _, a := range handler.attrs {
		if a.Key == "fields" {
			fields, _ = a.Value.String(), a.Key
		}
	}
	if fields == "" {
		t.Fatal("the audit must log the fields attr")
	}
	if !utf8.ValidString(fields) {
		t.Fatalf("logged field names must be valid UTF-8: %q", fields)
	}
	if strings.Contains(fields, longField) {
		t.Fatalf("the 64-byte cap must truncate the long field name: %q", fields)
	}
}
