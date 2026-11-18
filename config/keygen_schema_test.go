package config

import (
	"testing"
)

func TestPrintSchema_ValidOutput(t *testing.T) {
	schema, err := PrintSchema()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if schema == "" {
		t.Fatal("expected non-empty schema")
	}
	if len(schema) < 100 {
		t.Errorf("expected schema length > 100, got %d", len(schema))
	}
}

func TestBuiltInProviders_NotEmpty(t *testing.T) {
	providers := BuiltInProviders()
	if len(providers) == 0 {
		t.Fatal("expected non-empty built-in providers")
	}

	expected := []string{
		"anthropic", "gemini", "ollama",
		"deepseek", "mistral", "xai", "groq",
	}

	for _, name := range expected {
		if _, ok := providers[name]; !ok {
			t.Errorf("expected built-in provider %q", name)
		}
	}
}

func TestBuiltInProviders_HasFormatURLs(t *testing.T) {
	providers := BuiltInProviders()

	for name, pc := range providers {
		t.Run(name, func(t *testing.T) {
			if pc.URL == "" {
				t.Errorf("provider %q has empty URL", name)
			}
			if pc.AuthStyle == "" {
				t.Errorf("provider %q has empty AuthStyle", name)
			}
		})
	}
}
