package providers

import (
	"testing"
)

func TestGet_ExistingProvider(t *testing.T) {
	cases := []string{"gemini", "zai", "groq", "deepseek", "together", "openai", "github", "openrouter", "sambanova", "cerebras"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			spec, ok := Get(name)
			if !ok {
				t.Fatalf("expected provider %q to exist", name)
			}
			if spec.SupportsAutoToolChoice && spec.SupportsContentArrays {
				return
			}
		})
	}
}

func TestGet_NonExistingProvider(t *testing.T) {
	_, ok := Get("nonexistent")
	if ok {
		t.Fatal("expected false for nonexistent provider")
	}
}

func TestSupportsStreamOptions(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"deepseek", true},
		{"DeepSeek", true},
		{"zai", true},
		{"openrouter", true},
		{"nvidia", false},
		{"groq", false},
		{"gemini", false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := SupportsStreamOptions(tt.provider)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupportsAutoToolChoice(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"gemini", true},
		{"deepseek", true},
		{"zai", true},
		{"nvidia", false},
		{"groq", false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := SupportsAutoToolChoice(tt.provider)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupportsContentArrays(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"gemini", true},
		{"deepseek", true},
		{"openrouter", true},
		{"nvidia", false},
		{"groq", false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := SupportsContentArrays(tt.provider)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
