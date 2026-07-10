package util

import (
	"testing"
)

func TestAnthropicVersion_Standard(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantFam string
		wantMaj int
		wantMin int
		wantOK  bool
	}{
		{"opus-4-7", "claude-opus-4-7", "opus", 4, 7, true},
		{"opus-4-8", "claude-opus-4-8", "opus", 4, 8, true},
		{"sonnet-5", "claude-sonnet-5", "sonnet", 5, 0, true},
		{"sonnet-4-6", "claude-sonnet-4-6", "sonnet", 4, 6, true},
		{"haiku-3-5", "claude-haiku-3-5", "haiku", 3, 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fam, maj, min, ok := AnthropicVersion(tt.model)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if fam != tt.wantFam {
				t.Errorf("family = %v, want %v", fam, tt.wantFam)
			}
			if maj != tt.wantMaj {
				t.Errorf("major = %v, want %v", maj, tt.wantMaj)
			}
			if min != tt.wantMin {
				t.Errorf("minor = %v, want %v", min, tt.wantMin)
			}
		})
	}
}

func TestAnthropicVersion_Vertex(t *testing.T) {
	fam, maj, min, ok := AnthropicVersion("claude-opus-4-8@default")
	if !ok {
		t.Fatalf("ok = false")
	}
	if fam != "opus" {
		t.Errorf("family = %v, want opus", fam)
	}
	if maj != 4 || min != 8 {
		t.Errorf("version = %d.%d, want 4.8", maj, min)
	}
}

func TestAnthropicVersion_SAP(t *testing.T) {
	tests := []struct {
		model   string
		wantFam string
		wantMaj int
		wantMin int
	}{
		{"claude-4.7-opus", "opus", 4, 7},
		{"claude-5.0-sonnet", "sonnet", 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			fam, maj, min, ok := AnthropicVersion(tt.model)
			if !ok {
				t.Fatalf("ok = false")
			}
			if fam != tt.wantFam {
				t.Errorf("family = %v, want %v", fam, tt.wantFam)
			}
			if maj != tt.wantMaj || min != tt.wantMin {
				t.Errorf("version = %d.%d, want %d.%d", maj, min, tt.wantMaj, tt.wantMin)
			}
		})
	}
}

func TestAnthropicVersion_EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"empty string", ""},
		{"missing hyphen", "claude-opus"},
		{"wrong family", "claude-custom-4-7"},
		{"malformed version", "claude-opus-x-7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, ok := AnthropicVersion(tt.model)
			if ok {
				t.Errorf("ok = true, want false")
			}
		})
	}
}

func TestIsAnthropicAtLeast(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantFam   string
		wantMajor int
		wantMinor int
		wantTrue  bool
	}{
		{"exact match", "claude-opus-4-7", "opus", 4, 7, true},
		{"higher major", "claude-opus-5-0", "opus", 4, 7, true},
		{"higher minor", "claude-opus-4-8", "opus", 4, 7, true},
		{"lower major", "claude-opus-3-5", "opus", 4, 7, false},
		{"lower minor", "claude-opus-4-6", "opus", 4, 7, false},
		{"wrong family", "claude-sonnet-5", "opus", 4, 7, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAnthropicAtLeast(tt.model, tt.wantFam, tt.wantMajor, tt.wantMinor)
			if got != tt.wantTrue {
				t.Errorf("got %v, want %v", got, tt.wantTrue)
			}
		})
	}
}

func TestIsAnthropicOpus47OrLater(t *testing.T) {
	tests := []struct {
		model  string
		wantOK bool
	}{
		{"claude-opus-4-7", true},
		{"claude-opus-4-8", true},
		{"claude-opus-4-6", false},
		{"claude-sonnet-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := IsAnthropicOpus47OrLater(tt.model)
			if got != tt.wantOK {
				t.Errorf("got %v, want %v", got, tt.wantOK)
			}
		})
	}
}

func TestIsAnthropicSonnet5OrLater(t *testing.T) {
	tests := []struct {
		model  string
		wantOK bool
	}{
		{"claude-sonnet-5", true},
		{"claude-sonnet-5-1", true},
		{"claude-sonnet-4-7", false},
		{"claude-opus-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := IsAnthropicSonnet5OrLater(tt.model)
			if got != tt.wantOK {
				t.Errorf("got %v, want %v", got, tt.wantOK)
			}
		})
	}
}
