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
