package gateway

import (
	"testing"

	"github.com/nenya/config"
)

// TestMissingRequiredHeaderCaseInsensitive pins the AUD-19 fix (NENYA-223):
// HTTP header names are case-insensitive, so a server declaring
// required_headers: ["Authorization"] must be satisfied by a configured
// "authorization" (and vice versa). Pre-fix the exact map lookup made the
// mismatch permanent: the server never connected and every request selecting
// it failed closed with 503.
func TestMissingRequiredHeaderCaseInsensitive(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		headers  map[string]string
		want     string // "" = satisfied
	}{
		{
			name:     "exact spelling",
			required: []string{"Authorization"},
			headers:  map[string]string{"Authorization": "Bearer x"},
			want:     "",
		},
		{
			name:     "required canonical, configured lowercase",
			required: []string{"Authorization"},
			headers:  map[string]string{"authorization": "Bearer x"},
			want:     "",
		},
		{
			name:     "required lowercase, configured canonical",
			required: []string{"authorization"},
			headers:  map[string]string{"Authorization": "Bearer x"},
			want:     "",
		},
		{
			name:     "required UPPER, configured mixed",
			required: []string{"X-API-KEY"},
			headers:  map[string]string{"x-Api-Key": "k"},
			want:     "",
		},
		{
			name:     "genuinely absent",
			required: []string{"Authorization"},
			headers:  map[string]string{"X-Other": "v"},
			want:     "Authorization",
		},
		{
			name:     "present but blank",
			required: []string{"Authorization"},
			headers:  map[string]string{"authorization": "   "},
			want:     "Authorization",
		},
		{
			name:     "second header missing",
			required: []string{"Authorization", "X-API-Key"},
			headers:  map[string]string{"authorization": "Bearer x"},
			want:     "X-API-Key",
		},
		{
			name:     "no requirements",
			required: nil,
			headers:  nil,
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.MCPServerConfig{
				RequiredHeaders: tc.required,
				Headers:         tc.headers,
			}
			if got := missingRequiredHeader(cfg); got != tc.want {
				t.Fatalf("missingRequiredHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMissingRequiredHeaderExplicitWins pins the tie-break: when a
// configuration carries two spellings, the exact-match entry is used.
func TestMissingRequiredHeaderExplicitWins(t *testing.T) {
	cfg := config.MCPServerConfig{
		RequiredHeaders: []string{"Authorization"},
		Headers: map[string]string{
			"authorization": "", // lowercase entry is blank
			"Authorization": "Bearer x",
		},
	}
	if got := missingRequiredHeader(cfg); got != "" {
		t.Fatalf("the exact-match spelling must win, got missing %q", got)
	}
}
