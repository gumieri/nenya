package pipeline

import (
	"testing"
)

func TestShannonEntropy(t *testing.T) {
	tests := []struct {
		token string
		want  float64
		tol   float64
	}{
		{"aaaaa", 0.0, 0.01},
		{"abcdef", 2.585, 0.01},
		{"abcabc", 1.585, 0.01},
		{"0123456789abcdef", 4.0, 0.01},
		{"", 0.0, 0.01},
	}

	for _, tt := range tests {
		got := ShannonEntropy(tt.token)
		if diff := abs(got - tt.want); diff > tt.tol {
			t.Errorf("ShannonEntropy(%q) = %f, want %f (diff %f)", tt.token, got, tt.want, diff)
		}
	}
}

func TestShannonEntropy_RandomStrings(t *testing.T) {
	got := ShannonEntropy("xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK")
	if got < 4.5 {
		t.Errorf("expected high entropy for random string, got %f", got)
	}

	got = ShannonEntropy("the quick brown fox jumps over the lazy dog")
	if got > 4.5 {
		t.Errorf("expected low entropy for English prose, got %f", got)
	}
}

func TestEntropyRedaction_HighEntropyToken(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "password is xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK end"
	got := f.RedactHighEntropy(input, label)

	if !contains(got, label) {
		t.Errorf("expected redaction in %q", got)
	}
	if contains(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected token to be removed, got %q", got)
	}
	if !contains(got, "password is ") {
		t.Errorf("expected surrounding text preserved, got %q", got)
	}
	if !contains(got, " end") {
		t.Errorf("expected trailing text preserved, got %q", got)
	}
}

func TestEntropyRedaction_LowEntropyText(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "the quick brown fox jumps over the lazy dog and walks away"
	got := f.RedactHighEntropy(input, label)

	if got != input {
		t.Errorf("expected no redaction, got %q", got)
	}
}

func TestEntropyRedaction_ShortToken(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "short xJ3kL9mN2pQ8rS text"
	got := f.RedactHighEntropy(input, label)

	if got != input {
		t.Errorf("expected no redaction for short token, got %q", got)
	}
}

func TestEntropyRedaction_CodeSpanPreservation(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "before\n```\nxJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK\n```\nafter"
	spans := DetectCodeFences(input)
	got := f.RedactHighEntropyPreservingCodeSpans(input, spans, label)

	if !contains(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected code span content preserved, got %q", got)
	}
}

func TestEntropyRedaction_CodeSpanRedactsProse(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "prose xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK more\n```\ncode_here\n```\nend"
	spans := DetectCodeFences(input)
	got := f.RedactHighEntropyPreservingCodeSpans(input, spans, label)

	if contains(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected prose token to be redacted, got %q", got)
	}
	if !contains(got, "code_here") {
		t.Errorf("expected code span content preserved, got %q", got)
	}
}

func TestEntropyRedaction_KeyValueIsolation(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "API_KEY=xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK"
	got := f.RedactHighEntropy(input, label)

	if !contains(got, "API_KEY=") {
		t.Errorf("expected key preserved, got %q", got)
	}
	if contains(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected value redacted, got %q", got)
	}
}

func TestEntropyRedaction_JWT(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	jwt := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWUsImlhdCI6MTUxNjIzOTAyMn0"
	input := "Authorization: Bearer " + jwt
	got := f.RedactHighEntropy(input, label)

	if contains(got, jwt) {
		t.Errorf("expected JWT to be redacted, got %q", got)
	}
	if !contains(got, "Authorization: Bearer ") {
		t.Errorf("expected header preserved, got %q", got)
	}
}

func TestEntropyRedaction_Base64Secret(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	b64 := "dXNlcjpwYXNzd29yZDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNA=="
	input := "secret=" + b64
	got := f.RedactHighEntropy(input, label)

	if contains(got, b64) {
		t.Errorf("expected base64 secret to be redacted, got %q", got)
	}
}

func TestEntropyRedaction_RepeatedChars(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "AAAAA AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA end"
	got := f.RedactHighEntropy(input, label)

	if contains(got, label) {
		t.Errorf("expected repeated chars to not be redacted, got %q", got)
	}
}

func TestEntropyRedaction_UUID(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "id=550e8400-e29b-41d4-a716-446655440000"
	got := f.RedactHighEntropy(input, label)

	uuid := "550e8400-e29b-41d4-a716-446655440000"
	parts := []string{uuid, "550e8400", "e29b-41d4-a716-446655440000"}
	found := false
	for _, part := range parts {
		if contains(got, part) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("UUID parts may be redacted but that's acceptable — got %q", got)
	}
}

func TestEntropyRedaction_MultipleTokens(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	input := "key1=xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK key2=yZ8mN4pQ7rS2vT5wA1kL9jB3fD6hG0eI2oK"
	got := f.RedactHighEntropy(input, label)

	if contains(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected first token redacted, got %q", got)
	}
	if contains(got, "yZ8mN4pQ7rS2vT5wA1kL9jB3fD6hG0eI2oK") {
		t.Errorf("expected second token redacted, got %q", got)
	}
	if !contains(got, "key1=") || !contains(got, " key2=") {
		t.Errorf("expected keys preserved, got %q", got)
	}
}

func TestEntropyRedaction_EmptyAndShort(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	label := "[REDACTED]"

	if got := f.RedactHighEntropy("", label); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}

	if got := f.RedactHighEntropy("hello", label); got != "hello" {
		t.Errorf("expected short string unchanged, got %q", got)
	}
}

func TestTokenizeForEntropy(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello world", 2},
		{"key=value", 2},
		{"a,b;c", 3},
		{"'quoted'", 1},
		{"a  b", 2},
		{"", 0},
		{"   ", 0},
		{"key:val", 2},
		{"(paren)", 1},
		{"a|b", 2},
	}

	for _, tt := range tests {
		spans := tokenizeForEntropy(tt.input)
		if len(spans) != tt.want {
			t.Errorf("tokenizeForEntropy(%q) = %d spans, want %d", tt.input, len(spans), tt.want)
		}
	}
}

func TestTokenizeForEntropy_SpanBoundaries(t *testing.T) {
	input := "abc def ghi"
	spans := tokenizeForEntropy(input)

	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}

	if spans[0].offset != 0 || spans[0].length != 3 {
		t.Errorf("span[0] = %+v, want {0,3}", spans[0])
	}
	if spans[1].offset != 4 || spans[1].length != 3 {
		t.Errorf("span[1] = %+v, want {4,3}", spans[1])
	}
	if spans[2].offset != 8 || spans[2].length != 3 {
		t.Errorf("span[2] = %+v, want {8,3}", spans[2])
	}
}

func TestNewEntropyFilter_Defaults(t *testing.T) {
	f := NewEntropyFilter(4.5, 20)
	if f.Threshold != 4.5 {
		t.Errorf("expected threshold 4.5, got %f", f.Threshold)
	}
	if f.MinTokenLen != 20 {
		t.Errorf("expected MinTokenLen 20, got %d", f.MinTokenLen)
	}
	if f.MaxTokenLen != 512 {
		t.Errorf("expected MaxTokenLen 512, got %d", f.MaxTokenLen)
	}
}

func TestNewEntropyFilter_ClampMinToken(t *testing.T) {
	f := NewEntropyFilter(4.5, 5)
	if f.MinTokenLen != 20 {
		t.Errorf("expected MinTokenLen clamped to 20, got %d", f.MinTokenLen)
	}
}

func TestNewEntropyFilter_ClampThreshold(t *testing.T) {
	f := NewEntropyFilter(0, 20)
	if f.Threshold != 4.5 {
		t.Errorf("expected threshold clamped to 4.5, got %f", f.Threshold)
	}
}

func TestHasRepeatedChars(t *testing.T) {
	if !hasRepeatedChars("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Error("expected repeated chars detected")
	}
	if hasRepeatedChars("xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG") {
		t.Error("expected non-repeated chars not detected")
	}
	if hasRepeatedChars("short") {
		t.Error("expected short string to not trigger")
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
