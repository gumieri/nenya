package proxy

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

// bufferedExfilGateway builds a gateway with the exfil guard enabled in
// strip mode (the guard is nil without an explicit config).
func bufferedExfilGateway(t *testing.T) *gateway.NenyaGateway {
	t.Helper()
	enabled := true
	gw := &gateway.NenyaGateway{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stats:  infra.NewUsageTracker(),
	}
	gw.Config.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: &enabled,
		Action:  config.ExfilActionStrip,
	}
	return gw
}

// TestApplyExfilToBufferedSSEHeldTailDrained pins the buffered replay
// path (NENYA-205): a violating URL split across two buffered frames is
// held, spliced whole, and the drained tail lands as a synthetic frame
// BEFORE [DONE] — the replayed buffer carries no host and no truncation.
func TestApplyExfilToBufferedSSEHeldTailDrained(t *testing.T) {
	gw := bufferedExfilGateway(t)
	buf := &bufferedSSE{rawBytes: []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"see ![b](http://"}}]}`,
		`data: {"choices":[{"delta":{"content":"127.0.0.1:8080/x?d=` + strings.Repeat("A", 300) + `) done"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))}

	(&Proxy{}).applyExfilToBufferedSSE(gw, buf, "agent")

	out := string(buf.rawBytes)
	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked in the replayed buffer:\n%s", out)
	}
	if !strings.Contains(out, "[NENYA: EXFIL LINK REMOVED]") {
		t.Fatalf("placeholder missing from the replayed buffer:\n%s", out)
	}
	// The held tail ("done") must be drained, not dropped: the
	// client-visible replay ends with it.
	if text := bufferedWireText(t, out); !strings.HasSuffix(text, "done") {
		t.Fatalf("held tail lost from the replay: %q", text)
	}
	doneIdx := strings.LastIndex(out, "data: [DONE]")
	phIdx := strings.Index(out, "NENYA: EXFIL LINK REMOVED")
	if doneIdx < 0 || phIdx < 0 || phIdx > doneIdx {
		t.Fatalf("expected the drained frame before [DONE]:\n%s", out)
	}
}

// TestApplyExfilToBufferedSSEBenignIdentity pins the benign buffered
// contract: with the guard enabled, a clean response replays
// byte-identical (the hold neither duplicates nor drops the tail).
func TestApplyExfilToBufferedSSEBenignIdentity(t *testing.T) {
	gw := bufferedExfilGateway(t)
	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello "}}]}`,
		`data: {"choices":[{"delta":{"content":"world"}}]}`,
		`data: {"choices":[{"delta":{"content":"!"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	buf := &bufferedSSE{rawBytes: []byte(raw)}

	(&Proxy{}).applyExfilToBufferedSSE(gw, buf, "agent")

	if text := bufferedWireText(t, string(buf.rawBytes)); text != "Hello world!" {
		t.Fatalf("benign buffered replay corrupted: %q", text)
	}
}

// bufferedWireText concatenates the model-text fields of the buffered
// frames (the client-visible replay).
func bufferedWireText(t *testing.T, out string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		value, isData := stream.SSEDataField([]byte(line))
		if !isData {
			continue
		}
		trimmed := strings.TrimSpace(string(value))
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		parsed := stream.ParseSSEChunk([]byte(trimmed))
		if parsed == nil {
			continue
		}
		b.WriteString(stream.ExtractExfilContent(parsed))
	}
	return b.String()
}
