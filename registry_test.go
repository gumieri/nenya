package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentModelsStringLookup(t *testing.T) {
	raw := `{
		"strategy": "fallback",
		"models": ["gemini-2.5-flash", "deepseek-chat", "gpt-4o"]
	}`

	var agent AgentConfig
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(agent.Models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(agent.Models))
	}

	tests := []struct {
		idx            int
		wantProvider   string
		wantMaxContext int
	}{
		{0, "gemini", 128000},
		{1, "deepseek", 128000},
		{2, "github", 8000},
	}
	for _, tc := range tests {
		m := agent.Models[tc.idx]
		if m.Provider != tc.wantProvider {
			t.Errorf("model[%d].provider = %q, want %q", tc.idx, m.Provider, tc.wantProvider)
		}
		if m.MaxContext != tc.wantMaxContext {
			t.Errorf("model[%d].max_context = %d, want %d", tc.idx, m.MaxContext, tc.wantMaxContext)
		}
	}
}

func TestAgentModelsObjectOverride(t *testing.T) {
	raw := `{
		"strategy": "fallback",
		"models": [
			{"provider": "ollama", "model": "qwen2.5-coder:7b", "max_context": 32000}
		]
	}`

	var agent AgentConfig
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(agent.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(agent.Models))
	}

	m := agent.Models[0]
	if m.Provider != "ollama" {
		t.Errorf("provider = %q, want %q", m.Provider, "ollama")
	}
	if m.Model != "qwen2.5-coder:7b" {
		t.Errorf("model = %q, want %q", m.Model, "qwen2.5-coder:7b")
	}
	if m.MaxContext != 32000 {
		t.Errorf("max_context = %d, want %d", m.MaxContext, 32000)
	}
}

func TestAgentModelsUnknownString(t *testing.T) {
	raw := `{
		"strategy": "fallback",
		"models": ["nonexistent-model-xyz"]
	}`

	var agent AgentConfig
	err := json.Unmarshal([]byte(raw), &agent)
	if err == nil {
		t.Fatal("expected error for unknown model string, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestAgentModelsMixedArray(t *testing.T) {
	raw := `{
		"strategy": "fallback",
		"models": [
			"gemini-2.5-flash",
			{"provider": "ollama", "model": "my-custom-model", "max_context": 16000},
			"llama-3.3-70b-versatile"
		]
	}`

	var agent AgentConfig
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(agent.Models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(agent.Models))
	}

	if agent.Models[0].Provider != "gemini" {
		t.Errorf("model[0].provider = %q, want %q", agent.Models[0].Provider, "gemini")
	}
	if agent.Models[1].Provider != "ollama" {
		t.Errorf("model[1].provider = %q, want %q", agent.Models[1].Provider, "ollama")
	}
	if agent.Models[2].Provider != "groq" {
		t.Errorf("model[2].provider = %q, want %q", agent.Models[2].Provider, "groq")
	}
	if agent.Models[2].MaxContext != 131072 {
		t.Errorf("model[2].max_context = %d, want %d", agent.Models[2].MaxContext, 131072)
	}
}

func TestResolveProviderRegistry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"github": {
				URL:       upstream.URL + "/chat/completions",
				AuthStyle: "none",
			},
		},
	}
	gw := newTestGateway(cfg)

	p := gw.resolveProvider("gpt-4o")
	if p == nil {
		t.Fatal("expected provider for gpt-4o, got nil")
	}
	if p.Name != "github" {
		t.Errorf("provider name = %q, want %q", p.Name, "github")
	}

	p = gw.resolveProvider("nonexistent-model")
	if p != nil {
		t.Errorf("expected nil for unknown model, got %q", p.Name)
	}
}
