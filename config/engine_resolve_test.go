package config

import (
	"strings"
	"testing"
)

func TestResolveEngineRef_InlineWithProvider(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			Name:           "openai",
			URL:            "https://api.openai.com/v1",
			TimeoutSeconds: 30,
			AuthStyle:      "bearer",
			ApiFormat:      "openai",
		},
	}

	ref := EngineRef{
		Provider: "openai",
		Model:    "gpt-4",
	}

	targets, err := ResolveEngineRef(ref, nil, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Engine.Provider != "openai" {
		t.Errorf("expected provider 'openai', got %q", targets[0].Engine.Provider)
	}
	if targets[0].Engine.Model != "gpt-4" {
		t.Errorf("expected model 'gpt-4', got %q", targets[0].Engine.Model)
	}
	if targets[0].Engine.TimeoutSeconds != 30 {
		t.Errorf("expected timeout 30, got %d", targets[0].Engine.TimeoutSeconds)
	}
	if targets[0].Provider.URL != "https://api.openai.com/v1" {
		t.Errorf("expected URL 'https://api.openai.com/v1', got %q", targets[0].Provider.URL)
	}
}

func TestResolveEngineRef_InlineWithNoProvider(t *testing.T) {
	ref := EngineRef{
		Provider: "nonexistent",
		Model:    "gpt-4",
	}

	_, err := ResolveEngineRef(ref, nil, make(map[string]*Provider))
	if err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

func TestResolveEngineRef_InlineWithOverride(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			URL:            "https://api.openai.com/v1",
			TimeoutSeconds: 30,
		},
	}

	ref := EngineRef{
		Provider:       "openai",
		Model:          "gpt-4",
		TimeoutSeconds: 60,
		SystemPrompt:   "custom prompt",
	}

	targets, err := ResolveEngineRef(ref, nil, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets[0].Engine.TimeoutSeconds != 60 {
		t.Errorf("expected timeout 60 (override), got %d", targets[0].Engine.TimeoutSeconds)
	}
	if targets[0].Engine.SystemPrompt != "custom prompt" {
		t.Errorf("expected system prompt 'custom prompt', got %q", targets[0].Engine.SystemPrompt)
	}
}

func TestResolveEngineRef_AgentEngine(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			Name:  "openai",
			URL:   "https://api.openai.com/v1",
			AuthStyle: "bearer",
		},
	}

	agents := map[string]AgentConfig{
		"my-agent": {
			Models: []AgentModel{
				{Provider: "openai", Model: "gpt-4"},
			},
		},
	}

	ref := EngineRef{
		AgentName: "my-agent",
	}

	targets, err := ResolveEngineRef(ref, agents, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Engine.Provider != "openai" {
		t.Errorf("expected provider 'openai', got %q", targets[0].Engine.Provider)
	}
	if targets[0].Engine.Model != "gpt-4" {
		t.Errorf("expected model 'gpt-4', got %q", targets[0].Engine.Model)
	}
}

func TestResolveEngineRef_AgentNotFound(t *testing.T) {
	ref := EngineRef{
		AgentName: "nonexistent-agent",
	}

	_, err := ResolveEngineRef(ref, map[string]AgentConfig{}, map[string]*Provider{})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestResolveEngineRef_AgentNoModels(t *testing.T) {
	ref := EngineRef{
		AgentName: "empty-agent",
	}

	agents := map[string]AgentConfig{
		"empty-agent": {
			Models: []AgentModel{},
		},
	}

	_, err := ResolveEngineRef(ref, agents, map[string]*Provider{})
	if err == nil {
		t.Error("expected error for agent with no models")
	}
}

func TestResolveEngineRef_AgentAllModelsFail(t *testing.T) {
	ref := EngineRef{
		AgentName: "broken-agent",
	}

	agents := map[string]AgentConfig{
		"broken-agent": {
			Models: []AgentModel{
				{Provider: "nonexistent", Model: "gpt-4"},
			},
		},
	}

	_, err := ResolveEngineRef(ref, agents, map[string]*Provider{})
	if err == nil || !strings.Contains(err.Error(), "no models could be resolved") {
		t.Errorf("expected 'no models could be resolved', got: %v", err)
	}
}

func TestResolveSystemPrompts(t *testing.T) {
	ref := EngineRef{SystemPrompt: "ref prompt"}
	agent := AgentConfig{SystemPrompt: "agent prompt", SystemPromptFile: "agent.txt"}

	prompt, file := resolveSystemPrompts(ref, agent)
	if prompt != "ref prompt" || file != "" {
		t.Errorf("expected ('ref prompt', ''), got (%q, %q)", prompt, file)
	}

	ref2 := EngineRef{}
	prompt2, file2 := resolveSystemPrompts(ref2, agent)
	if prompt2 != "agent prompt" || file2 != "agent.txt" {
		t.Errorf("expected ('agent prompt', 'agent.txt'), got (%q, %q)", prompt2, file2)
	}
}

func TestResolveTimeout(t *testing.T) {
	tests := []struct {
		refTimeout     int
		providerTimeout int
		want           int
	}{
		{60, 30, 60},
		{0, 30, 30},
		{0, 0, 0},
	}
	for _, tt := range tests {
		ref := EngineRef{TimeoutSeconds: tt.refTimeout}
		got := resolveTimeout(ref, tt.providerTimeout)
		if got != tt.want {
			t.Errorf("resolveTimeout(%+v, %d) = %d, want %d", ref, tt.providerTimeout, got, tt.want)
		}
	}
}

func TestGetProviderURL(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {URL: "https://api.openai.com/v1"},
	}

	if got := getProviderURL("openai", "", providers); got != "https://api.openai.com/v1" {
		t.Errorf("expected provider URL, got %q", got)
	}
	if got := getProviderURL("", "https://custom.url", providers); got != "https://custom.url" {
		t.Errorf("expected custom URL, got %q", got)
	}
	if got := getProviderURL("nonexistent", "", providers); got != "" {
		t.Errorf("expected empty for unknown, got %q", got)
	}
}

func TestGetProviderTimeout(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {TimeoutSeconds: 30},
	}
	if got := getProviderTimeout("openai", providers); got != 30 {
		t.Errorf("expected 30, got %d", got)
	}
	if got := getProviderTimeout("nonexistent", providers); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestGetInlineProviderConfig(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {URL: "https://api.openai.com/v1", TimeoutSeconds: 30},
	}
	url, timeout := getInlineProviderConfig(EngineRef{Provider: "openai"}, providers)
	if url != "https://api.openai.com/v1" || timeout != 30 {
		t.Errorf("expected (URL, 30), got (%q, %d)", url, timeout)
	}

	url, timeout = getInlineProviderConfig(EngineRef{Provider: ""}, providers)
	if url != "" || timeout != 0 {
		t.Errorf("expected ('', 0) for empty provider, got (%q, %d)", url, timeout)
	}

	url, timeout = getInlineProviderConfig(EngineRef{Provider: "nonexistent"}, providers)
	if url != "" || timeout != 0 {
		t.Errorf("expected ('', 0) for unknown provider, got (%q, %d)", url, timeout)
	}
}

func TestBuildProvider(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			URL:       "https://api.openai.com/v1",
			AuthStyle: "bearer",
			ApiFormat: "openai",
		},
	}

	p := buildProvider("openai", "https://api.openai.com/v1", 30, providers)
	if p.Name != "openai" {
		t.Errorf("expected name 'openai', got %q", p.Name)
	}
	if p.APIKey != "" {
		t.Errorf("expected empty API key, got %q", p.APIKey)
	}
	if p.AuthStyle != "bearer" {
		t.Errorf("expected auth_style 'bearer', got %q", p.AuthStyle)
	}
	if p.ApiFormat != "openai" {
		t.Errorf("expected api_format 'openai', got %q", p.ApiFormat)
	}
}

func TestGetProviderAuthStyle(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {AuthStyle: "bearer"},
	}
	if got := getProviderAuthStyle("openai", providers); got != "bearer" {
		t.Errorf("expected 'bearer', got %q", got)
	}
	if got := getProviderAuthStyle("unknown", providers); got != "" {
		t.Errorf("expected '', got %q", got)
	}
}

func TestGetProviderApiFormat(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {ApiFormat: "openai"},
	}
	if got := getProviderApiFormat("openai", providers); got != "openai" {
		t.Errorf("expected 'openai', got %q", got)
	}
	if got := getProviderApiFormat("unknown", providers); got != "" {
		t.Errorf("expected '', got %q", got)
	}
}

func TestResolveSingleEngineRef(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			URL: "https://api.openai.com/v1",
		},
	}

	ref := &EngineRef{
		AgentName: "",
		Provider:  "",
	}
	err := resolveSingleEngineRef(ref, nil, providers, "test")
	if err != nil {
		t.Errorf("expected no error for empty ref, got: %v", err)
	}
}

func TestResolveSingleEngineRef_WithProvider(t *testing.T) {
	providers := map[string]*Provider{
		"openai": {
			URL: "https://api.openai.com/v1",
		},
	}

	ref := &EngineRef{
		Provider: "openai",
		Model:    "gpt-4",
	}
	err := resolveSingleEngineRef(ref, nil, providers, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ref.ResolvedTargets) != 1 {
		t.Errorf("expected 1 resolved target, got %d", len(ref.ResolvedTargets))
	}
}
