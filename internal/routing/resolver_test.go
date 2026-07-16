package routing

import (
	"strings"
	"testing"

	"github.com/nenya/config"
)

func providers() map[string]*config.Provider {
	builtIn := config.BuiltInProviders()
	keys := make(map[string]string)
	for name := range builtIn {
		keys[name] = "test-key"
	}
	return config.ResolveProviders(&config.Config{Providers: builtIn}, &config.SecretsConfig{ProviderKeys: keys})
}

func TestResolveProvider_KnownModels(t *testing.T) {
	p := providers()

	cases := []struct {
		model    string
		provider string
	}{
		{"gemini-2.5-flash", "gemini"},
		{"gemini-3.1-flash-lite-preview", "gemini"},
		{"deepseek-v4-flash", "deepseek"},
		{"deepseek-v4-pro", "deepseek"},
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

	got = DetermineUpstream("deepseek-v4-flash", p)
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

	got := ProviderURL("gemini", "", "", nil, p)
	expected := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestProviderURL_UnknownProvider(t *testing.T) {
	p := providers()

	got := ProviderURL("nonexistent", "", "", nil, p)
	if got != "" {
		t.Fatalf("expected empty string for unknown provider, got %q", got)
	}
}

func TestProviderURL_AgentURLOverride(t *testing.T) {
	p := providers()

	got := ProviderURL("gemini", "https://custom.example.com/v1/chat/completions", "", nil, p)
	if got != "https://custom.example.com/v1/chat/completions" {
		t.Fatalf("expected agent URL override, got %q", got)
	}

	got = ProviderURL("nonexistent", "https://override.example.com", "", nil, p)
	if got != "https://override.example.com" {
		t.Fatalf("expected agent URL override even for unknown provider, got %q", got)
	}
}

func TestUpstreamTarget_LogValue_RedactsCredential(t *testing.T) {
	target := UpstreamTarget{
		URL:         "https://api.example.com/v1/chat/completions",
		Model:       "claude-sonnet-4-5",
		Format:      "anthropic",
		Provider:    "anthropic",
		Credential:  "sk-ant-super-secret-key-1234567890",
		AccountName: "test-account",
		MaxContext:  200000,
		MaxOutput:   8192,
		CoolKey:     "agent:anthropic:claude-sonnet-4-5",
	}

	val := target.LogValue()
	rendered := val.String()

	if strings.Contains(rendered, "sk-ant-super-secret-key") {
		t.Fatalf("LogValue leaked credential: %s", rendered)
	}
	if strings.Contains(rendered, "1234567890") {
		t.Fatalf("LogValue leaked credential fragment: %s", rendered)
	}

	for _, field := range []string{"provider=", "model=", "format=", "url=", "account=", "reasoning_effort="} {
		if !strings.Contains(rendered, field) {
			t.Fatalf("LogValue missing expected field %q in: %s", field, rendered)
		}
	}
}

func TestUpstreamTarget_LogValue_EmptyFields(t *testing.T) {
	target := UpstreamTarget{Credential: "secret-key"}
	val := target.LogValue()
	rendered := val.String()

	if strings.Contains(rendered, "secret-key") {
		t.Fatalf("LogValue leaked credential for empty target: %s", rendered)
	}

	for _, field := range []string{"provider=", "model=", "format=", "url=", "account=", "reasoning_effort="} {
		if !strings.Contains(rendered, field) {
			t.Fatalf("LogValue missing expected field key %q for empty target in: %s", field, rendered)
		}
	}
}
