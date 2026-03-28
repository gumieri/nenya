package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestDetermineUpstream(t *testing.T) {
	cfg := Config{
		Upstream: UpstreamConfig{
			GeminiURL:   defaultGeminiURL,
			DeepSeekURL: defaultDeepSeekURL,
			ZaiURL:      defaultZaiURL,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	tests := []struct {
		model    string
		expected string
	}{
		{"gemini-3.1-flash-lite", defaultGeminiURL},
		{"gemini-3-flash", defaultGeminiURL},
		{"deepseek-reasoner", defaultDeepSeekURL},
		{"deepseek-chat", defaultDeepSeekURL},
		{"glm-5", defaultZaiURL},
		{"unknown", defaultZaiURL},
	}

	for _, tt := range tests {
		result := g.determineUpstream(tt.model)
		if result != tt.expected {
			t.Errorf("For model %s expected %s, got %s", tt.model, tt.expected, result)
		}
	}
}

func TestTransformRequestForUpstream(t *testing.T) {
	cfg := Config{
		Upstream: UpstreamConfig{},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	tests := []struct {
		name         string
		upstreamURL  string
		body         string
		expectedBody string
		expectError  bool
	}{
		{
			name:         "Gemini 3 flash maps to preview API ID",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-3-flash", "messages": [{"role": "user", "content": "test"}]}`,
			expectedBody: `{"messages":[{"content":"test","role":"user"}],"model":"gemini-3-flash-preview"}`,
		},
		{
			name:         "Gemini 3.1 flash-lite maps to preview API ID",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-3.1-flash-lite", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-3.1-flash-lite-preview"}`,
		},
		{
			name:         "Gemini 2.5-flash passes through unchanged",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-2.5-flash", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-2.5-flash"}`,
		},
		{
			name:         "Gemini generic flash alias maps to gemini-2.5-flash",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-flash", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-2.5-flash"}`,
		},
		{
			name:         "Unknown Gemini model passes through unchanged",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-4-ultra-future", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-4-ultra-future"}`,
		},
		{
			name:         "DeepSeek no transformation",
			upstreamURL:  "https://api.deepseek.com/v1/chat/completions",
			body:         `{"model": "deepseek-reasoner", "messages": []}`,
			expectedBody: `{"messages":[],"model":"deepseek-reasoner"}`,
		},
		{
			name:         "z.ai no transformation",
			upstreamURL:  "https://api.z.ai/v1/chat/completions",
			body:         `{"model": "glm-5", "messages": []}`,
			expectedBody: `{"messages":[],"model":"glm-5"}`,
		},
		{
			name:        "Invalid JSON returns error",
			upstreamURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:        `{invalid json`,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transformedBody, finalModel, err := g.transformRequestForUpstream(tt.upstreamURL, []byte(tt.body), "")
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			// Parse transformed body to compare model field
			var transformed map[string]interface{}
			if err := json.Unmarshal(transformedBody, &transformed); err != nil {
				t.Errorf("Failed to unmarshal transformed body: %v", err)
				return
			}

			// Parse expected body
			var expected map[string]interface{}
			if err := json.Unmarshal([]byte(tt.expectedBody), &expected); err != nil {
				t.Errorf("Failed to unmarshal expected body: %v", err)
				return
			}

			// Compare model fields
			transformedModel, _ := transformed["model"].(string)
			expectedModel, _ := expected["model"].(string)
			if transformedModel != expectedModel {
				t.Errorf("Expected model %s, got %s", expectedModel, transformedModel)
			}

			// If finalModel is returned, check it matches
			if finalModel != "" && finalModel != transformedModel {
				t.Errorf("finalModel %s doesn't match transformed model %s", finalModel, transformedModel)
			}
		})
	}
}

func TestReplaceModel(t *testing.T) {
	g := newTestGateway(Config{Upstream: UpstreamConfig{}})

	body := []byte(`{"model":"old-model","messages":[{"role":"user","content":"hi"}]}`)
	result, _, err := g.transformRequestForUpstream("https://api.z.ai/v1/chat/completions", body, "new-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if payload["model"] != "new-model" {
		t.Errorf("expected model 'new-model', got %v", payload["model"])
	}
	// Original body unchanged
	var orig map[string]interface{}
	json.Unmarshal(body, &orig)
	if orig["model"] != "old-model" {
		t.Errorf("original body was mutated")
	}
}

func TestReplaceModelInvalidJSON(t *testing.T) {
	g := newTestGateway(Config{Upstream: UpstreamConfig{}})

	_, _, err := g.transformRequestForUpstream("https://api.z.ai/v1/chat/completions", []byte(`not json`), "model")
	if err == nil {
		t.Errorf("expected error for invalid JSON, got nil")
	}
}

func TestProviderURL(t *testing.T) {
	cfg := Config{
		Upstream: UpstreamConfig{
			GeminiURL:   defaultGeminiURL,
			DeepSeekURL: defaultDeepSeekURL,
			ZaiURL:      defaultZaiURL,
			GroqURL:     defaultGroqURL,
			TogetherURL: defaultTogetherURL,
		},
	}
	g := newTestGateway(cfg)

	cases := []struct{ provider, want string }{
		{"gemini", defaultGeminiURL},
		{"deepseek", defaultDeepSeekURL},
		{"zai", defaultZaiURL},
		{"groq", defaultGroqURL},
		{"together", defaultTogetherURL},
		{"ollama", defaultOllamaOpenAIURL},
		{"unknown", defaultZaiURL}, // falls back to z.ai
	}
	for _, c := range cases {
		got := g.providerURL(c.provider, "")
		if got != c.want {
			t.Errorf("providerURL(%q) = %q, want %q", c.provider, got, c.want)
		}
	}

	// URL override takes precedence.
	custom := "http://custom-host:9090/v1/chat/completions"
	got := g.providerURL("gemini", custom)
	if got != custom {
		t.Errorf("URL override not honoured: got %q", got)
	}
}

func TestBuildTargetListRoundRobin(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-pro"},
			{Provider: "gemini", Model: "gemini-2.5-flash"},
			{Provider: "zai", Model: "glm-5"},
		},
	}
	cfg := Config{
		Upstream: UpstreamConfig{
			GeminiURL:   defaultGeminiURL,
			DeepSeekURL: defaultDeepSeekURL,
			ZaiURL:      defaultZaiURL,
		},
	}
	g := newTestGateway(cfg)

	// First call: start = 0 → gemini-2.5-pro, gemini-2.5-flash, glm-5
	t1 := g.buildTargetList("test", agent, 0)
	if len(t1) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(t1))
	}
	if t1[0].model != "gemini-2.5-pro" {
		t.Errorf("first call: expected gemini-2.5-pro first, got %s", t1[0].model)
	}

	// Second call: start = 1 → gemini-2.5-flash, glm-5, gemini-2.5-pro
	t2 := g.buildTargetList("test", agent, 0)
	if t2[0].model != "gemini-2.5-flash" {
		t.Errorf("second call: expected gemini-2.5-flash first, got %s", t2[0].model)
	}

	// Third call: start = 2 → glm-5, gemini-2.5-pro, gemini-2.5-flash
	t3 := g.buildTargetList("test", agent, 0)
	if t3[0].model != "glm-5" {
		t.Errorf("third call: expected glm-5 first, got %s", t3[0].model)
	}
}

func TestBuildTargetListCooldownSkip(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-pro"},
			{Provider: "gemini", Model: "gemini-2.5-flash"},
		},
	}
	cfg := Config{
		Upstream: UpstreamConfig{
			GeminiURL:   defaultGeminiURL,
			DeepSeekURL: defaultDeepSeekURL,
			ZaiURL:      defaultZaiURL,
		},
	}
	g := newTestGateway(cfg)

	// Put gemini-2.5-pro in cooldown.
	g.agentMu.Lock()
	g.modelCooldowns["gemini:gemini-2.5-pro"] = time.Now().Add(60 * time.Second)
	g.agentMu.Unlock()

	targets := g.buildTargetList("test", agent, 0)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (cooling at end), got %d", len(targets))
	}
	// gemini-2.5-flash should be first (active), gemini-2.5-pro at end (cooling).
	if targets[0].model != "gemini-2.5-flash" {
		t.Errorf("expected gemini-2.5-flash first (active), got %s", targets[0].model)
	}
	if targets[1].model != "gemini-2.5-pro" {
		t.Errorf("expected gemini-2.5-pro last (cooling), got %s", targets[1].model)
	}
}

func TestBuildTargetListCounterIsolation(t *testing.T) {
	// Two different agents should have independent counters.
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "A"},
			{Provider: "gemini", Model: "B"},
		},
	}
	cfg := Config{
		Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL},
	}
	g := newTestGateway(cfg)

	// Advance agent1 counter twice.
	g.buildTargetList("agent1", agent, 0)
	g.buildTargetList("agent1", agent, 0)

	// agent2 counter is independent — should still start at 0 → model A first.
	t2 := g.buildTargetList("agent2", agent, 0)
	if t2[0].model != "A" {
		t.Errorf("agent2 counter should be independent: expected A first, got %s", t2[0].model)
	}
}

func TestBuildTargetListCooldownExpiry(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-pro"},
		},
	}
	cfg := Config{
		Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL},
	}
	g := &NenyaGateway{
		config:         cfg,
		secrets:        &SecretsConfig{},
		rateLimits:     make(map[string]*rateLimiter),
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
		agentMu:        sync.Mutex{},
	}

	// Set cooldown that has already expired.
	g.agentMu.Lock()
	g.modelCooldowns["gemini:gemini-2.5-pro"] = time.Now().Add(-1 * time.Second)
	g.agentMu.Unlock()

	targets := g.buildTargetList("test", agent, 0)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	// Expired cooldown → model should be in the active list (not cooling).
	if targets[0].coolKey != "gemini:gemini-2.5-pro" {
		t.Errorf("unexpected coolKey: %s", targets[0].coolKey)
	}
}

func TestBuildTargetListFallbackStrategy(t *testing.T) {
	agent := AgentConfig{
		Strategy: "fallback",
		Models: []AgentModel{
			{Provider: "gemini", Model: "A"},
			{Provider: "gemini", Model: "B"},
			{Provider: "gemini", Model: "C"},
		},
	}
	cfg := Config{Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL}}
	g := newTestGateway(cfg)

	// Fallback strategy: every call should start with model A regardless of how many calls are made.
	for i := 0; i < 5; i++ {
		targets := g.buildTargetList("test", agent, 0)
		if len(targets) != 3 {
			t.Fatalf("call %d: expected 3 targets, got %d", i+1, len(targets))
		}
		if targets[0].model != "A" {
			t.Errorf("call %d: fallback strategy should always start with A, got %s", i+1, targets[0].model)
		}
	}
}

func TestBuildTargetListRoundRobinStrategy(t *testing.T) {
	agent := AgentConfig{
		Strategy: "round-robin",
		Models: []AgentModel{
			{Provider: "gemini", Model: "A"},
			{Provider: "gemini", Model: "B"},
		},
	}
	cfg := Config{Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL}}
	g := newTestGateway(cfg)

	// Round-robin: starting model should rotate.
	first := g.buildTargetList("test", agent, 0)[0].model  // start=0 → A
	second := g.buildTargetList("test", agent, 0)[0].model // start=1 → B
	if first == second {
		t.Errorf("round-robin should rotate starting model, but both calls returned %s", first)
	}
}

func TestBuildTargetListMaxContext(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "small", MaxContext: 100},
			{Provider: "gemini", Model: "large"},
		},
	}
	cfg := Config{Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL}}
	g := newTestGateway(cfg)

	// Request with 50 tokens: both models available.
	targets := g.buildTargetList("test", agent, 50)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets for 50 tokens, got %d", len(targets))
	}

	// Request with 200 tokens: "small" (max_context=100) should be excluded.
	targets = g.buildTargetList("test", agent, 200)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target for 200 tokens (small excluded), got %d", len(targets))
	}
	if targets[0].model != "large" {
		t.Errorf("expected model 'large', got %s", targets[0].model)
	}
}

func TestBuildTargetListMaxContextNoLimit(t *testing.T) {
	// max_context = 0 means no limit — model should never be excluded.
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "unlimited", MaxContext: 0},
		},
	}
	cfg := Config{Upstream: UpstreamConfig{GeminiURL: defaultGeminiURL}}
	g := newTestGateway(cfg)

	targets := g.buildTargetList("test", agent, 1_000_000)
	if len(targets) != 1 {
		t.Errorf("max_context=0 should impose no limit, got %d targets", len(targets))
	}
}
