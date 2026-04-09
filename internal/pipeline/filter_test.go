package pipeline

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"nenya/internal/config"
)

func TestRedactSecrets(t *testing.T) {
	awsKey := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	tokenPat := regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`)
	label := "[REDACTED]"

	tests := []struct {
		name     string
		text     string
		enabled  bool
		patterns []*regexp.Regexp
		want     string
	}{
		{
			name:     "filter disabled returns original",
			text:     "key=AKIAIOSFODNN7EXAMPLE",
			enabled:  false,
			patterns: []*regexp.Regexp{awsKey},
			want:     "key=AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "AWS key redaction with pattern",
			text:     "key=AKIAIOSFODNN7EXAMPLE end",
			enabled:  true,
			patterns: []*regexp.Regexp{awsKey},
			want:     "key=[REDACTED] end",
		},
		{
			name:     "multiple patterns",
			text:     "aws=AKIAIOSFODNN7EXAMPLE gh=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij",
			enabled:  true,
			patterns: []*regexp.Regexp{awsKey, tokenPat},
			want:     "aws=[REDACTED] gh=[REDACTED]",
		},
		{
			name:     "no match returns original",
			text:     "nothing to see here",
			enabled:  true,
			patterns: []*regexp.Regexp{awsKey},
			want:     "nothing to see here",
		},
		{
			name:     "empty patterns returns original",
			text:     "some text with AKIAIOSFODNN7EXAMPLE",
			enabled:  true,
			patterns: []*regexp.Regexp{},
			want:     "some text with AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "nil patterns returns original",
			text:     "some text with AKIAIOSFODNN7EXAMPLE",
			enabled:  true,
			patterns: nil,
			want:     "some text with AKIAIOSFODNN7EXAMPLE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSecrets(tt.text, tt.enabled, tt.patterns, label)
			if got != tt.want {
				t.Errorf("RedactSecrets() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateMiddleOut(t *testing.T) {
	cfg := config.GovernanceConfig{
		KeepFirstPercent: 40,
		KeepLastPercent:  40,
	}
	sep := "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"
	sepLen := utf8.RuneCountInString(sep)

	tests := []struct {
		name    string
		text    string
		maxSize int
		want    string
	}{
		{
			name:    "short text no truncation",
			text:    "hello",
			maxSize: 100,
			want:    "hello",
		},
		{
			name:    "exact length no truncation",
			text:    "hello",
			maxSize: 5,
			want:    "hello",
		},
		{
			name:    "long text truncated with separator",
			text:    strings.Repeat("a", 50) + "MIDDLE" + strings.Repeat("b", 50),
			maxSize: 50,
			want: func() string {
				available := 50 - sepLen
				keepFirst := int(float64(available) * 40 / 100)
				keepLast := int(float64(available) * 40 / 100)
				if keepFirst+keepLast > available {
					total := keepFirst + keepLast
					keepFirst = keepFirst * available / total
					keepLast = available - keepFirst
				}
				if keepFirst == 0 && keepLast > 0 {
					keepFirst = 1
					keepLast = available - 1
				} else if keepLast == 0 && keepFirst > 0 {
					keepLast = 1
					keepFirst = available - 1
				}
				runes := []rune(strings.Repeat("a", 50) + "MIDDLE" + strings.Repeat("b", 50))
				return string(runes[:keepFirst]) + sep + string(runes[len(runes)-keepLast:])
			}(),
		},
		{
			name:    "zero max size returns truncated separator",
			text:    strings.Repeat("x", 1000),
			maxSize: 0,
			want:    "",
		},
		{
			name:    "one rune max",
			text:    strings.Repeat("x", 100),
			maxSize: 1,
			want:    string([]rune(sep)[0]),
		},
		{
			name:    "empty text",
			text:    "",
			maxSize: 100,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateMiddleOut(tt.text, tt.maxSize, cfg)
			if got != tt.want {
				t.Errorf("TruncateMiddleOut() = %q, want %q", got, tt.want)
			}
		})
	}
}
