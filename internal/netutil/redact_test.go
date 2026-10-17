package netutil

import (
	"strings"
	"testing"
)

// TestRedactURLUserinfo pins the URL credential redaction, including the
// defensive forms whose userinfo does not parse as userinfo.
func TestRedactURLUserinfo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no userinfo", "http://proxy.invalid:3128", "http://proxy.invalid:3128"},
		{"userinfo", "http://user:secret@proxy.invalid:3128", "http://[REDACTED]@proxy.invalid:3128"},
		{"user only", "socks5://user@proxy.invalid:1080", "socks5://[REDACTED]@proxy.invalid:1080"},
		{"opaque credential", "proxy:MARKER@host", "[REDACTED]@host"},
		{"unparseable with userinfo", "http://user:secret@ho st:11434", "http://[REDACTED]@ho st:11434"},
		{"path with at", "http://proxy.invalid/some@path", "http://proxy.invalid/some@path"},
		{"query with at", "http://proxy.invalid/?a=b@c", "http://proxy.invalid/?a=b@c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURLUserinfo(tc.raw)
			if got != tc.want {
				t.Fatalf("RedactURLUserinfo(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "MARKER") {
				t.Errorf("redaction left credential material in %q", got)
			}
		})
	}
}
