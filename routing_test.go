package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestDetermineUpstream(t *testing.T) {
	bp := builtInProviders()
	cfg := Config{
		Providers: bp,
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		model    string
		expected string
	}{
		{"gemini-3.1-flash-lite", bp["gemini"].URL},
		{"gemini-3-flash", bp["gemini"].URL},
		{"deepseek-reasoner", bp["deepseek"].URL},
		{"deepseek-chat", bp["deepseek"].URL},
		{"glm-5", bp["zai"].URL},
		{"glm-4.7-flash", bp["zai"].URL},
		{"zai-coding-plan/glm-5", bp["zai"].URL},
		{"zai-coding-plan/glm-5.1", bp["zai"].URL},
		{"zai-coding-plan/glm-4.7-flash", bp["zai"].URL},
		{"unknown", bp["zai"].URL},
	}

	for _, tt := range tests {
		result := g.determineUpstream(tt.model)
		if result != tt.expected {
			t.Errorf("For model %s expected %s, got %s", tt.model, tt.expected, result)
		}
	}
}

func TestTransformRequestForUpstream(t *testing.T) {
	cfg := Config{Providers: builtInProviders()}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		name         string
		provider     string
		upstreamURL  string
		body         string
		expectedBody string
		expectError  bool
		noTransform  bool
	}{
		{
			name:         "Gemini 3 flash maps to preview API ID",
			provider:     "gemini",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-3-flash", "messages": [{"role": "user", "content": "test"}]}`,
			expectedBody: `{"messages":[{"content":"test","role":"user"}],"model":"gemini-3-flash-preview"}`,
		},
		{
			name:         "Gemini 3.1 flash-lite maps to preview API ID",
			provider:     "gemini",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-3.1-flash-lite", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-3.1-flash-lite-preview"}`,
		},
		{
			name:         "Gemini 2.5-flash passes through unchanged",
			provider:     "gemini",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-2.5-flash", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-2.5-flash"}`,
		},
		{
			name:         "Gemini generic flash alias maps to gemini-2.5-flash",
			provider:     "gemini",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-flash", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-2.5-flash"}`,
		},
		{
			name:         "Unknown Gemini model passes through unchanged",
			provider:     "gemini",
			upstreamURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:         `{"model": "gemini-4-ultra-future", "messages": []}`,
			expectedBody: `{"messages":[],"model":"gemini-4-ultra-future"}`,
		},
		{
			name:         "DeepSeek no transformation",
			provider:     "deepseek",
			upstreamURL:  "https://api.deepseek.com/v1/chat/completions",
			body:         `{"model": "deepseek-reasoner", "messages": []}`,
			expectedBody: `{"messages":[],"model":"deepseek-reasoner"}`,
		},
		{
			name:         "z.ai no transformation",
			provider:     "zai",
			upstreamURL:  "https://api.z.ai/v1/chat/completions",
			body:         `{"model": "glm-5", "messages": []}`,
			expectedBody: `{"messages":[],"model":"glm-5"}`,
		},
		{
			name:        "No model field returns nil",
			provider:    "gemini",
			upstreamURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:        `{}`,
			noTransform: true,
		},
		{
			name:        "Non-string model returns nil",
			provider:    "gemini",
			upstreamURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			body:        `{"model":123}`,
			noTransform: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(tt.body), &payload); err != nil {
				t.Fatalf("Failed to parse test body: %v", err)
			}
			transformedBody, finalModel, err := g.transformRequestForUpstream(tt.provider, tt.upstreamURL, payload, "")
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

			if tt.noTransform {
				if transformedBody != nil {
					t.Errorf("Expected nil body for %s, got %s", tt.name, string(transformedBody))
				}
				if finalModel != "" {
					t.Errorf("Expected empty finalModel for %s, got %q", tt.name, finalModel)
				}
				return
			}

			var transformed map[string]interface{}
			if err := json.Unmarshal(transformedBody, &transformed); err != nil {
				t.Errorf("Failed to unmarshal transformed body: %v", err)
				return
			}

			var expected map[string]interface{}
			if err := json.Unmarshal([]byte(tt.expectedBody), &expected); err != nil {
				t.Errorf("Failed to unmarshal expected body: %v", err)
				return
			}

			transformedModel, _ := transformed["model"].(string)
			expectedModel, _ := expected["model"].(string)
			if transformedModel != expectedModel {
				t.Errorf("Expected model %s, got %s", expectedModel, transformedModel)
			}

			if finalModel != "" && finalModel != transformedModel {
				t.Errorf("finalModel %s doesn't match transformed model %s", finalModel, transformedModel)
			}
		})
	}
}

func TestReplaceModel(t *testing.T) {
	g := newTestGateway(Config{})

	payload := map[string]interface{}{
		"model": "old-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	}
	result, _, err := g.transformRequestForUpstream("zai", "https://api.z.ai/v1/chat/completions", payload, "new-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p map[string]interface{}
	if err := json.Unmarshal(result, &p); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if p["model"] != "new-model" {
		t.Errorf("expected model 'new-model', got %v", p["model"])
	}
	if payload["model"] != "old-model" {
		t.Errorf("original payload was mutated: got %v", payload["model"])
	}
}

func TestReplaceModelNoMutation(t *testing.T) {
	g := newTestGateway(Config{})

	payload := map[string]interface{}{
		"model":    "original",
		"messages": []interface{}{},
	}

	_, _, err := g.transformRequestForUpstream("zai", "https://api.z.ai/v1/chat/completions", payload, "overridden")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["model"] != "original" {
		t.Errorf("payload mutated after transform: got %v", payload["model"])
	}
}

func TestProviderURL(t *testing.T) {
	bp := builtInProviders()
	cfg := Config{
		Providers: bp,
	}
	g := newTestGateway(cfg)

	cases := []struct{ provider, want string }{
		{"gemini", bp["gemini"].URL},
		{"deepseek", bp["deepseek"].URL},
		{"zai", bp["zai"].URL},
		{"groq", bp["groq"].URL},
		{"together", bp["together"].URL},
		{"ollama", bp["ollama"].URL},
		{"unknown", ""},
	}
	for _, c := range cases {
		got := g.providerURL(c.provider, "")
		if got != c.want {
			t.Errorf("providerURL(%q) = %q, want %q", c.provider, got, c.want)
		}
	}

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
			{Provider: "zai", Model: "zai-coding-plan/glm-5"},
		},
	}
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	t1 := g.buildTargetList("test", agent, 0)
	if len(t1) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(t1))
	}
	if t1[0].model != "gemini-2.5-pro" {
		t.Errorf("first call: expected gemini-2.5-pro first, got %s", t1[0].model)
	}

	t2 := g.buildTargetList("test", agent, 0)
	if t2[0].model != "gemini-2.5-flash" {
		t.Errorf("second call: expected gemini-2.5-flash first, got %s", t2[0].model)
	}

	t3 := g.buildTargetList("test", agent, 0)
	if t3[0].model != "zai-coding-plan/glm-5" {
		t.Errorf("third call: expected zai-coding-plan/glm-5 first, got %s", t3[0].model)
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
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	g.agentMu.Lock()
	g.modelCooldowns["test:gemini:gemini-2.5-pro"] = time.Now().Add(60 * time.Second)
	g.agentMu.Unlock()

	targets := g.buildTargetList("test", agent, 0)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (cooling at end), got %d", len(targets))
	}
	if targets[0].model != "gemini-2.5-flash" {
		t.Errorf("expected gemini-2.5-flash first (active), got %s", targets[0].model)
	}
	if targets[1].model != "gemini-2.5-pro" {
		t.Errorf("expected gemini-2.5-pro last (cooling), got %s", targets[1].model)
	}
}

func TestBuildTargetListCounterIsolation(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "A"},
			{Provider: "gemini", Model: "B"},
		},
	}
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	g.buildTargetList("agent1", agent, 0)
	g.buildTargetList("agent1", agent, 0)

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
		Providers: builtInProviders(),
	}
	g := &NenyaGateway{
		config:         cfg,
		secrets:        &SecretsConfig{},
		providers:      resolveProviders(&cfg, &SecretsConfig{}),
		rateLimits:     make(map[string]*rateLimiter),
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
		agentMu:        sync.Mutex{},
		stats:          NewUsageTracker(),
	}

	g.agentMu.Lock()
	g.modelCooldowns["test:gemini:gemini-2.5-pro"] = time.Now().Add(-1 * time.Second)
	g.agentMu.Unlock()

	targets := g.buildTargetList("test", agent, 0)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].coolKey != "test:gemini:gemini-2.5-pro" {
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
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

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
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	first := g.buildTargetList("test", agent, 0)[0].model
	second := g.buildTargetList("test", agent, 0)[0].model
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
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	targets := g.buildTargetList("test", agent, 50)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets for 50 tokens, got %d", len(targets))
	}

	targets = g.buildTargetList("test", agent, 200)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target for 200 tokens (small excluded), got %d", len(targets))
	}
	if targets[0].model != "large" {
		t.Errorf("expected model 'large', got %s", targets[0].model)
	}
}

func TestBuildTargetListMaxContextNoLimit(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "gemini", Model: "unlimited", MaxContext: 0},
		},
	}
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	targets := g.buildTargetList("test", agent, 1_000_000)
	if len(targets) != 1 {
		t.Errorf("max_context=0 should impose no limit, got %d targets", len(targets))
	}
}

func TestBuildTargetListUnknownProvider(t *testing.T) {
	agent := AgentConfig{
		Models: []AgentModel{
			{Provider: "nonexistent", Model: "m1"},
		},
	}
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := newTestGateway(cfg)

	targets := g.buildTargetList("test", agent, 0)
	if len(targets) != 0 {
		t.Errorf("expected 0 targets for unknown provider, got %d", len(targets))
	}
}

func TestResolveWindowMaxContext(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		model   string
		targets []upstreamTarget
		want    int
	}{
		{
			name: "agent with max_context on first model",
			cfg: Config{
				Agents: map[string]AgentConfig{
					"my-agent": {
						Models: []AgentModel{
							{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 1000000},
						},
					},
				},
			},
			model:   "my-agent",
			targets: nil,
			want:    1000000,
		},
		{
			name: "agent with max_context=0 skips",
			cfg: Config{
				Agents: map[string]AgentConfig{
					"my-agent": {
						Models: []AgentModel{
							{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 0},
							{Provider: "gemini", Model: "gemini-2.5-pro", MaxContext: 2000000},
						},
					},
				},
			},
			model:   "my-agent",
			targets: nil,
			want:    2000000,
		},
		{
			name: "agent with all max_context=0 returns 0",
			cfg: Config{
				Agents: map[string]AgentConfig{
					"my-agent": {
						Models: []AgentModel{
							{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 0},
						},
					},
				},
			},
			model:   "my-agent",
			targets: nil,
			want:    0,
		},
		{
			name: "direct route matching prefix returns 0",
			cfg: Config{
				Providers: map[string]ProviderConfig{
					"gemini": {
						URL:           "https://gemini.example.com/v1/chat/completions",
						RoutePrefixes: []string{"gemini-"},
						AuthStyle:     "bearer",
					},
				},
			},
			model: "gemini-2.5-flash",
			targets: []upstreamTarget{
				{url: "https://gemini.example.com/v1/chat/completions", model: "gemini-2.5-flash", provider: "gemini"},
			},
			want: 0,
		},
		{
			name: "no agent and no prefix match returns 0",
			cfg: Config{
				Providers: builtInProviders(),
			},
			model: "unknown-model",
			targets: []upstreamTarget{
				{url: "https://z.ai/v1/chat/completions", model: "unknown-model", provider: "zai"},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := &SecretsConfig{}
			g := NewNenyaGateway(tt.cfg, secrets, slog.Default())
			got := g.resolveWindowMaxContext(tt.model, tt.targets)
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInjectAPIKey(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		apiKey      string
		authStyle   string
		wantErr     bool
		wantAuth    string
		wantGoogKey bool
	}{
		{"bearer with key", "test", "sk-123", "bearer", false, "Bearer sk-123", false},
		{"bearer missing key", "test", "", "bearer", true, "", false},
		{"bearer+x-goog with key", "test", "AIza...", "bearer+x-goog", false, "Bearer AIza...", true},
		{"bearer+x-goog missing key", "test", "", "bearer+x-goog", true, "", false},
		{"none style", "test", "", "none", false, "", false},
		{"unknown provider", "missing", "", "", true, "", false},
		{"unknown auth style", "test", "key", "digest", true, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Providers: map[string]ProviderConfig{
					"test": {URL: "http://example.com", RoutePrefixes: []string{"test-"}, AuthStyle: tt.authStyle},
				},
			}
			secrets := &SecretsConfig{ProviderKeys: map[string]string{"test": tt.apiKey}}
			g := NewNenyaGateway(cfg, secrets, slog.Default())

			headers := make(http.Header)
			err := g.injectAPIKey(tt.provider, headers)

			if (err != nil) != tt.wantErr {
				t.Errorf("error: got %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantAuth != "" {
				if got := headers.Get("Authorization"); got != tt.wantAuth {
					t.Errorf("Authorization: got %q, want %q", got, tt.wantAuth)
				}
			}
			if tt.wantGoogKey {
				if got := headers.Get("x-goog-api-key"); got != tt.apiKey {
					t.Errorf("x-goog-api-key: got %q, want %q", got, tt.apiKey)
				}
			}
		})
	}
}
