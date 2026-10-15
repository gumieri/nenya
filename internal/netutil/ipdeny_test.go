package netutil

import (
	"net"
	"testing"
)

// TestParseStandaloneIPv4 pins the policy boundary between a URL host (where a
// browser-style integer spelling is an address) and a standalone value (where a
// port, version, or mode must not be mistaken for one). NENYA-147.
func TestParseStandaloneIPv4(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"2130706433", false}, // a bare decimal is an id far more often than an address
		{"8080", false},
		{"2025", false},
		{"0644", false}, // a leading-zero mode
		{"0.5", false},  // two labels: a version
		{"10.0", false},
		{"127.0", false},
		{"0x7f000001", true}, // an explicit hex integer
		{"10.0.0", false},    // three labels: a semver
		{"127.1.2", false},
		{"127.1.2.3", true}, // a complete dotted IPv4
		{"0177.0.0.1", true},
		{"127.1", false}, // two labels only
		{"", false},
		{"example.com", false},
		{"999.999.999.999", false}, // overflow
	}
	for _, tc := range tests {
		_, ok := ParseStandaloneIPv4(tc.host)
		if ok != tc.want {
			t.Errorf("ParseStandaloneIPv4(%q) ok = %v, want %v", tc.host, ok, tc.want)
		}
	}
}

// TestParseNumericIPv4URLHost keeps the full browser-style interpretation for URL
// hosts, where the integer spellings are live bypass shapes.
func TestParseNumericIPv4URLHost(t *testing.T) {
	for host, want := range map[string]net.IP{
		"2130706433": net.IPv4(127, 0, 0, 1),
		"0x7f000001": net.IPv4(127, 0, 0, 1),
		"127.1":      net.IPv4(127, 0, 0, 1),
		"0177.0.0.1": net.IPv4(127, 0, 0, 1),
	} {
		ip, ok := ParseNumericIPv4(host)
		if !ok {
			t.Fatalf("ParseNumericIPv4(%q) failed", host)
		}
		if !ip.Equal(want) {
			t.Errorf("ParseNumericIPv4(%q) = %v, want %v", host, ip, want)
		}
	}
}

// TestDenyHostNormalization pins the normalization the URL policies rely on.
func TestDenyHostNormalization(t *testing.T) {
	for host, want := range map[string]string{
		"EXAMPLE.com.":    "example.com",
		"db.internal..":   "db.internal",
		"fe80::1%25eth0":  "fe80::1",
		"example.com%2ex": "example.com%2ex",
		"LOCALHOST":       "localhost",
		"10.0.0.5:8080/x": "10.0.0.5:8080/x",
	} {
		if got := NormalizeHost(host); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", host, got, want)
		}
	}
	if !DenyHost("db.internal") || !DenyHost("localhost") || !DenyHost("127.0.0.1") {
		t.Error("DenyHost must deny the special-use names and loopback")
	}
	if DenyHost("example.com") || DenyHost("93.184.216.34") {
		t.Error("DenyHost must not deny public destinations")
	}
}
