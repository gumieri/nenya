package pipeline

import (
	"net/http"
	"testing"
)

func TestClassifyClient(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		wantIDE  bool
		wantName string
	}{
		{"cursor via ua", map[string]string{"User-Agent": "Cursor/0.45.0"}, true, "cursor"},
		{"cursor mixed ua", map[string]string{"User-Agent": "Mozilla/5.0 Cursor/0.45.0 (darwin)"}, true, "cursor"},
		{"opencode via ua", map[string]string{"User-Agent": "opencode/1.0.0"}, true, "opencode"},
		{"opencode via editor-version", map[string]string{"Editor-Version": "OpenCode/1.0.0"}, true, "opencode"},
		{"opencode via editor-plugin-version", map[string]string{"Editor-Plugin-Version": "OpenCode/1.0.0"}, true, "opencode"},
		{"opencode via ua lowercase", map[string]string{"User-Agent": "OpenCode/2.0"}, true, "opencode"},
		{"unknown client", map[string]string{"User-Agent": "Mozilla/5.0 (unknown)"}, false, ""},
		{"empty headers", map[string]string{}, false, ""},
		{"curl", map[string]string{"User-Agent": "curl/8.0"}, false, ""},
		{"random header no match", map[string]string{"X-Custom": "opencode"}, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
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
