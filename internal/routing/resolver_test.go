package routing

import (
	"testing"

	"nenya/internal/config"
)

func providers() map[string]*config.Provider {
	builtIn := config.BuiltInProviders()
	return config.ResolveProviders(&config.Config{Providers: builtIn}, &config.SecretsConfig{ProviderKeys: map[string]string{}})
}

func TestGeminiModelMap(t *testing.T) {
	if len(GeminiModelMap) == 0 {
		t.Fatal("GeminiModelMap should not be empty")
	}
	if GeminiModelMap["gemini-flash"] != "gemini-2.5-flash" {
		t.Fatalf("expected gemini-flash -> gemini-2.5-flash, got %s", GeminiModelMap["gemini-flash"])
	}
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
		{"qwen2.5-72b-turbo", "together"},
	}

	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got := ResolveProvider(c.model, p)
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

	zai := ResolveProvider("glm-some-unknown-model", p)
	if zai == nil || zai.Name != "zai" {
		t.Fatalf("expected zai provider for glm-some-unknown-model, got %v", zai)
	}

	zai2 := ResolveProvider("zai-coding-plan/some-model", p)
	if zai2 == nil {
		t.Fatal("expected zai provider for zai-coding-plan/ prefix")
	}

	gemini := ResolveProvider("gemini-unknown-variant", p)
	if gemini == nil || gemini.Name != "gemini" {
		t.Fatalf("expected gemini provider for gemini-unknown-variant, got %v", gemini)
	}

	ds := ResolveProvider("deepseek-r1-xyz", p)
	if ds == nil || ds.Name != "deepseek" {
		t.Fatalf("expected deepseek provider for deepseek-r1-xyz, got %v", ds)
	}
}

func TestResolveProvider_UnknownNoMatch(t *testing.T) {
	p := providers()

	got := ResolveProvider("totally-unknown-model-no-prefix", p)
	if got != nil {
		t.Fatalf("expected nil for unknown model, got %q", got.Name)
	}
}

func TestResolveProvider_EmptyModel(t *testing.T) {
	p := providers()

	got := ResolveProvider("", p)
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

func TestDetermineUpstream_UnknownFallsBackToZAI(t *testing.T) {
	p := providers()

	got := DetermineUpstream("totally-unknown-no-prefix", p)
	expected := "https://api.z.ai/api/paas/v4/chat/completions"
	if got != expected {
		t.Fatalf("expected zai fallback %q, got %q", expected, got)
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

func TestIsGeminiProvider_True(t *testing.T) {
	p := providers()

	if !IsGeminiProvider("gemini", p) {
		t.Fatal("expected true for gemini provider")
	}
}

func TestIsGeminiProvider_False(t *testing.T) {
	p := providers()

	cases := []string{"deepseek", "zai", "groq", "together", "ollama"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if IsGeminiProvider(name, p) {
				t.Fatalf("expected false for %q", name)
			}
		})
	}
}

func TestIsGeminiProvider_Unknown(t *testing.T) {
	p := providers()

	if IsGeminiProvider("nonexistent", p) {
		t.Fatal("expected false for unknown provider")
	}
}

func TestIsGeminiProvider_NilProviders(t *testing.T) {
	if IsGeminiProvider("gemini", nil) {
		t.Fatal("expected false with nil providers")
	}
}

func TestIsZAIProvider_True(t *testing.T) {
	if !IsZAIProvider("zai") {
		t.Fatal("expected true for zai")
	}
}

func TestIsZAIProvider_False(t *testing.T) {
	cases := []string{"gemini", "deepseek", "groq", "together", "", "z ai", "ZAi"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if IsZAIProvider(name) {
				t.Fatalf("expected false for %q", name)
			}
		})
	}
}
