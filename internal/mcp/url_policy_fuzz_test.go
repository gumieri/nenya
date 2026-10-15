package mcp

import (
	"strings"
	"testing"
)

// FuzzDenyURLHost exercises the tool-argument URL policy over arbitrary string
// values (NENYA-147): the scanner must terminate without panicking, and a
// denied verdict must always carry a reason (so a caller can never see
// denied=true with an empty explanation).
func FuzzDenyURLHost(f *testing.F) {
	for _, seed := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://2130706433/",
		"http://0x7f000001/",
		"http://0177.0.0.1/",
		"http://127.1/",
		"http://[fe80::1%25eth0]/",
		"http://[64:ff9b::7f00:1]/",
		"http://localhost../",
		"http://%31%32%37.0.0.1/",
		"curl -sS http://10.0.0.5/x",
		"//10.0.0.5/x",
		"ftp://127.0.0.1/file",
		"169.254.169.254",
		"localhost",
		"https://example.com/ok",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		reason, denied := denyURLHost(value, []string{"example.com", "10.0.0.5", "*.internal"})
		if denied && strings.TrimSpace(reason) == "" {
			t.Fatalf("denied %q without a reason", value)
		}
		if !denied && reason != "" {
			t.Fatalf("clean %q returned reason %q", value, reason)
		}
		// The scanner must be stable: a second run yields the same verdict.
		againReason, againDenied := denyURLHost(value, []string{"example.com", "10.0.0.5", "*.internal"})
		if againDenied != denied || againReason != reason {
			t.Fatalf("non-deterministic verdict for %q: (%v,%q) vs (%v,%q)", value, denied, reason, againDenied, againReason)
		}
	})
}
