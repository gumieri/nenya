package config

import (
	"log/slog"
	"net/http"
	"testing"
)

func TestOllamaHealthURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:11434/api/generate", "http://localhost:11434/api/tags"},
		{"http://localhost:11434/v1/chat/completions", "http://localhost:11434/api/tags"},
		{"http://localhost:11434", "http://localhost:11434"},
		{"http://ollama:11434/v1/chat/completions", "http://ollama:11434/api/tags"},
		{"http://ollama:11434/api/generate", "http://ollama:11434/api/tags"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := OllamaHealthURL(tt.input)
			if got != tt.want {
				t.Errorf("OllamaHealthURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestApplyAuthHeader(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		p := &Provider{AuthStyle: "bearer", APIKey: "sk-test"}
		if err := ApplyAuthHeader(req, p); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("bearer+x-goog", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		p := &Provider{AuthStyle: "bearer+x-goog", APIKey: "key123"}
		if err := ApplyAuthHeader(req, p); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer key123" {
			t.Errorf("Authorization: got %q", got)
		}
		if got := req.Header.Get("x-goog-api-key"); got != "key123" {
			t.Errorf("x-goog-api-key: got %q", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		p := &Provider{AuthStyle: "none"}
		if err := ApplyAuthHeader(req, p); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("expected no Authorization header")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		p := &Provider{AuthStyle: "bearer", APIKey: ""}
		if err := ApplyAuthHeader(req, p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer " {
			t.Errorf("got %q", got)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		p := &Provider{AuthStyle: "ntlm"}
		if err := ApplyAuthHeader(req, p); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestValidatePatterns(t *testing.T) {
	t.Run("all valid", func(t *testing.T) {
		logger := slog.Default()
		err := ValidatePatterns("test", []string{`[0-9]+`, `hello`}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("invalid pattern", func(t *testing.T) {
		logger := slog.Default()
		err := ValidatePatterns("test", []string{`[0-9`}, logger)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("mixed", func(t *testing.T) {
		logger := slog.Default()
		err := ValidatePatterns("test", []string{`[0-9]+`, `[invalid`}, logger)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestGetValidationEndpoint(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			"https://generativelanguage.googleapis.com/v1beta/models"},
		{"https://api.deepseek.com/chat/completions",
			"https://api.deepseek.com/models"},
		{"https://api.z.ai/api/paas/v4/chat/completions",
			"https://api.z.ai/v1/models"},
		{"https://api.groq.com/openai/v1/chat/completions",
			"https://api.groq.com/openai/v1/models"},
		{"https://api.together.xyz/v1/chat/completions",
			"https://api.together.xyz/v1/models"},
		{"https://api.openai.com/v1/chat/completions",
			"https://api.openai.com/v1/models"},
		{"http://127.0.0.1:11434/v1/chat/completions", ""},
		{"http://localhost:11434/v1/chat/completions", ""},
		{"https://custom.example.com/v1/chat/completions",
			"https://custom.example.com/v1/models"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := GetValidationEndpoint(tt.url)
			if got != tt.want {
				t.Errorf("GetValidationEndpoint(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
