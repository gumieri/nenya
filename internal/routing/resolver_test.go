package routing

import (
	"testing"

	"nenya/internal/config"
)

func providers() map[string]*config.Provider {
	builtIn := config.BuiltInProviders()
	return config.ResolveProviders(&config.Config{Providers: builtIn}, &config.SecretsConfig{ProviderKeys: map[string]string{}})
}

func TestResolveProvider_KnownModels(t *testing.T) {
	p := providers()

	cases := []struct {
		model    string
		provider string
	}{
		{"gemini-2.5-flash", "gemini"},
		{"gemini-3.1-flash-lite-preview", "gemini"},
		{"deepseek-chat", "deepseek"},
		{"deepseek-reasoner", "deepseek"},
		{"glm-5", "zai"},
		{"glm-4.7-flash", "zai"},
		{"nemotron-3-super", "nvidia_free"},
		{"qwen-3.6-plus", "qwen_free"},
		{"minimax-m2.5", "minimax_free"},
		{"llama-3.3-70b-versatile", "groq"},
		{"mixtral-8x7b-32768", "groq"},
		{"llama-3.1-405b-instruct", "sambanova"},
		{"llama-3.3-70b", "cerebras"},
		{"gpt-4o", "github"},
		{"phi-3.5-mini-instruct", "github"},
		{"qwen2.5-72b-turbo", "together"},
		{"claude-opus-4-5", "anthropic"},
		{"claude-sonnet-4-5", "anthropic"},
		{"claude-haiku-4-5", "anthropic"},
		{"claude-3-5-haiku-latest", "anthropic"},
		{"mistral-large-latest", "mistral"},
		{"codestral-latest", "mistral"},
		{"grok-4", "xai"},
		{"grok-3-mini", "xai"},
		{"sonar-pro", "perplexity"},
	}

	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got := ResolveProvider(c.model, p, nil)
			if got == nil {
				t.Fatalf("expected provider %q for model %q, got nil", c.provider, c.model)
			}
			if got.Name != c.provider {
				t.Fatalf("expected provider %q, got %q", c.provider, got.Name)
			}
		})
	}
}

func TestResolveProvider_PrefixMatch(t *testing.T) {
	p := providers()

	zai := ResolveProvider("glm-some-unknown-model", p, nil)
	if zai == nil || zai.Name != "zai" {
		t.Fatalf("expected zai provider for glm-some-unknown-model, got %v", zai)
	}

	gemini := ResolveProvider("gemini-unknown-variant", p, nil)
	if gemini == nil || gemini.Name != "gemini" {
		t.Fatalf("expected gemini provider for gemini-unknown-variant, got %v", gemini)
	}

	ds := ResolveProvider("deepseek-r1-xyz", p, nil)
	if ds == nil || ds.Name != "deepseek" {
		t.Fatalf("expected deepseek provider for deepseek-r1-xyz, got %v", ds)
	}

	claude := ResolveProvider("claude-some-unknown-variant", p, nil)
	if claude == nil || claude.Name != "anthropic" {
		t.Fatalf("expected anthropic provider for claude-some-unknown-variant, got %v", claude)
	}

	mistral := ResolveProvider("mistral-some-unknown-variant", p, nil)
	if mistral == nil || mistral.Name != "mistral" {
		t.Fatalf("expected mistral provider for mistral-some-unknown-variant, got %v", mistral)
	}

	grok := ResolveProvider("grok-some-unknown-variant", p, nil)
	if grok == nil || grok.Name != "xai" {
		t.Fatalf("expected xai provider for grok-some-unknown-variant, got %v", grok)
	}
}

func TestResolveProvider_UnknownNoMatch(t *testing.T) {
	p := providers()

	got := ResolveProvider("totally-unknown-model-no-prefix", p, nil)
	if got != nil {
		t.Fatalf("expected nil for unknown model, got %q", got.Name)
	}
}

func TestResolveProvider_EmptyModel(t *testing.T) {
	p := providers()

	got := ResolveProvider("", p, nil)
	if got != nil {
		t.Fatalf("expected nil for empty model, got %q", got.Name)
	}
}

func TestDetermineUpstream_KnownModels(t *testing.T) {
	p := providers()

	got := DetermineUpstream("gemini-2.5-flash", p)
	expected := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}

	got = DetermineUpstream("deepseek-chat", p)
	expected = "https://api.deepseek.com/chat/completions"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestDetermineUpstream_UnknownNoMatch(t *testing.T) {
	p := providers()

	got := DetermineUpstream("totally-unknown-no-prefix", p)
	if got != "" {
		t.Fatalf("expected empty string for unknown model, got %q", got)
	}
}

func TestDetermineUpstream_NoProviders(t *testing.T) {
	got := DetermineUpstream("anything", nil)
	if got != "" {
		t.Fatalf("expected empty string with no providers, got %q", got)
	}

	got = DetermineUpstream("anything", map[string]*config.Provider{})
	if got != "" {
		t.Fatalf("expected empty string with empty providers, got %q", got)
	}
}

func TestProviderURL_KnownProvider(t *testing.T) {
	p := providers()

	got := ProviderURL("gemini", "", p)
	expected := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestProviderURL_UnknownProvider(t *testing.T) {
	p := providers()

	got := ProviderURL("nonexistent", "", p)
	if got != "" {
		t.Fatalf("expected empty string for unknown provider, got %q", got)
	}
}

func TestProviderURL_AgentURLOverride(t *testing.T) {
	p := providers()

	got := ProviderURL("gemini", "https://custom.example.com/v1/chat/completions", p)
	if got != "https://custom.example.com/v1/chat/completions" {
		t.Fatalf("expected agent URL override, got %q", got)
	}

	got = ProviderURL("nonexistent", "https://override.example.com", p)
	if got != "https://override.example.com" {
		t.Fatalf("expected agent URL override even for unknown provider, got %q", got)
	}
}
