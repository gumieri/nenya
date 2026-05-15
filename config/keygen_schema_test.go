package config

import (
	"testing"
)

func TestGenerateToken(t *testing.T) {
	token := GenerateToken()
	if len(token) != 51 {
		t.Errorf("expected token length 51 (nk- + 48 hex chars), got %d", len(token))
	}
	if token[:3] != "nk-" {
		t.Errorf("expected 'nk-' prefix, got %s", token[:3])
	}

	token2 := GenerateToken()
	if token == token2 {
		t.Error("tokens should be unique")
	}
}

func TestValidateKeyID_Valid(t *testing.T) {
	tests := []string{
		"my-key",
		"my-key-1",
		"test123",
		"a",
		"a-b-c-d-e-f",
	}

	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			if err := ValidateKeyID(id); err != nil {
				t.Errorf("unexpected error for %q: %v", id, err)
			}
		})
	}
}

func TestValidateKeyID_Invalid(t *testing.T) {
	tests := []struct {
		id  string
		msg string
	}{
		{id: "", msg: "empty"},
		{id: "UPPERCASE", msg: "uppercase"},
		{id: "my-key_1", msg: "underscore"},
		{id: "my key", msg: "space"},
		{id: "key.with.dots", msg: "dots"},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if err := ValidateKeyID(tt.id); err == nil {
				t.Errorf("expected error for %q", tt.id)
			}
		})
	}
}

func TestValidateKeyID_TooLong(t *testing.T) {
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateKeyID(string(long)); err == nil {
		t.Error("expected error for key ID longer than 64 chars")
	}
}

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
