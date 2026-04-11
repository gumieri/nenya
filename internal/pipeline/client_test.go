package pipeline

import (
	"net/http"
	"testing"
)

func TestClassifyClient(t *testing.T) {
	tests := []struct {
		name     string
		ua       string
		wantIDE  bool
		wantName string
	}{
		{"cursor ide", "Cursor/0.45.0", true, "cursor"},
		{"cursor with extras", "Mozilla/5.0 Cursor/0.45.0 (darwin)", true, "cursor"},
		{"opencode ide", "opencode/1.0.0", true, "opencode"},
		{"opencode lowercase", "OpenCode/2.0", true, "opencode"},
		{"unknown client", "Mozilla/5.0 (unknown)", false, ""},
		{"empty ua", "", false, ""},
		{"curl", "curl/8.0", false, ""},
		{"no header", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.ua != "" {
				h.Set("User-Agent", tt.ua)
			}
			got := ClassifyClient(h)
			if got.IsIDE != tt.wantIDE {
				t.Errorf("IsIDE = %v, want %v", got.IsIDE, tt.wantIDE)
			}
			if got.ClientName != tt.wantName {
				t.Errorf("ClientName = %q, want %q", got.ClientName, tt.wantName)
			}
		})
	}
}
