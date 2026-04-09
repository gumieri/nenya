package routing

import (
	"log/slog"
	"testing"
	"time"

	"nenya/internal/config"
)

func targetProviders() map[string]*config.Provider {
	builtIn := config.BuiltInProviders()
	return config.ResolveProviders(&config.Config{Providers: builtIn}, &config.SecretsConfig{ProviderKeys: map[string]string{}})
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNewAgentState(t *testing.T) {
	a := NewAgentState(testLogger())
	if a == nil {
		t.Fatal("NewAgentState returned nil")
	}
	if a.Counters == nil {
		t.Fatal("Counters should be initialized")
	}
	if a.CB == nil {
		t.Fatal("CB should be initialized")
	}
	if a.ActiveCooldowns() != 0 {
		t.Fatal("new agent should have no active cooldowns")
	}
}

func TestBuildTargetList_RoundRobin(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-flash"},
			{Provider: "deepseek", Model: "deepseek-chat"},
			{Provider: "zai", Model: "glm-5"},
		},
	}

	t1 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if len(t1) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(t1))
	}
	if t1[0].Provider != "gemini" {
		t.Fatalf("first call: expected gemini first, got %s", t1[0].Provider)
	}

	t2 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if len(t2) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(t2))
	}
	if t2[0].Provider != "deepseek" {
		t.Fatalf("second call: expected deepseek first, got %s", t2[0].Provider)
	}

	t3 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if t3[0].Provider != "zai" {
		t.Fatalf("third call: expected zai first, got %s", t3[0].Provider)
	}

	t4 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if t4[0].Provider != "gemini" {
		t.Fatalf("fourth call: expected gemini first (wrap), got %s", t4[0].Provider)
	}
}

func TestBuildTargetList_CooldownSkip(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-flash"},
			{Provider: "deepseek", Model: "deepseek-chat"},
			{Provider: "zai", Model: "glm-5"},
		},
	}

	t1 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	geminiTarget := t1[0]

	a.ActivateCooldown(geminiTarget, 10*time.Minute)

	t2 := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if len(t2) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(t2))
	}
	if t2[0].Provider == "gemini" {
		t.Fatal("gemini should be moved to end due to cooldown")
	}
	if t2[2].Provider != "gemini" {
		t.Fatalf("expected gemini at end, got %s", t2[2].Provider)
	}
}

func TestBuildTargetList_FallbackStrategy(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Strategy: "fallback",
		Models: []config.AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-flash"},
			{Provider: "deepseek", Model: "deepseek-chat"},
		},
	}

	for i := 0; i < 5; i++ {
		targets := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
		if targets[0].Provider != "gemini" {
			t.Fatalf("iteration %d: fallback strategy should always start with gemini, got %s", i, targets[0].Provider)
		}
	}
}

func TestBuildTargetList_UnknownProviderSkipped(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-flash"},
			{Provider: "nonexistent_provider", Model: "some-model"},
			{Provider: "deepseek", Model: "deepseek-chat"},
		},
	}

	targets := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (unknown provider skipped), got %d", len(targets))
	}
	for _, tgt := range targets {
		if tgt.Provider == "nonexistent_provider" {
			t.Fatal("unknown provider should have been skipped")
		}
	}
}

func TestBuildTargetList_EmptyModels(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{}

	targets := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if targets != nil {
		t.Fatalf("expected nil for empty models, got %v", targets)
	}
}

func TestBuildTargetList_TokenCountExceedsMaxContext(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "nvidia_free", Model: "nemotron-3-super"},
			{Provider: "gemini", Model: "gemini-2.5-flash"},
		},
	}

	targets := a.BuildTargetList(testLogger(), "test-agent", agent, 5000, p)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (nemotron skipped), got %d", len(targets))
	}
	if targets[0].Provider != "gemini" {
		t.Fatalf("expected gemini, got %s", targets[0].Provider)
	}
}

func TestBuildTargetList_MaxContextFromAgentModel(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 500},
			{Provider: "deepseek", Model: "deepseek-chat"},
		},
	}

	targets := a.BuildTargetList(testLogger(), "test-agent", agent, 1000, p)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (gemini skipped due to agent-level max_context), got %d", len(targets))
	}
	if targets[0].Provider != "deepseek" {
		t.Fatalf("expected deepseek, got %s", targets[0].Provider)
	}
}

func TestBuildTargetList_TargetFields(t *testing.T) {
	p := targetProviders()
	a := NewAgentState(testLogger())
	agent := config.AgentConfig{
		Models: []config.AgentModel{
			{Provider: "deepseek", Model: "deepseek-chat"},
		},
	}

	targets := a.BuildTargetList(testLogger(), "my-agent", agent, 1000, p)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}

	tgt := targets[0]
	if tgt.URL != "https://api.deepseek.com/chat/completions" {
		t.Fatalf("unexpected URL: %s", tgt.URL)
	}
	if tgt.Model != "deepseek-chat" {
		t.Fatalf("unexpected Model: %s", tgt.Model)
	}
	if tgt.Provider != "deepseek" {
		t.Fatalf("unexpected Provider: %s", tgt.Provider)
	}
	if tgt.CoolKey != "my-agent:deepseek:deepseek-chat" {
		t.Fatalf("unexpected CoolKey: %s", tgt.CoolKey)
	}
	if tgt.MaxOutput != 8192 {
		t.Fatalf("unexpected MaxOutput: %d", tgt.MaxOutput)
	}
}

func TestActivateCooldown_Active(t *testing.T) {
	a := NewAgentState(testLogger())
	target := UpstreamTarget{
		CoolKey: "agent:gemini:gemini-2.5-flash",
	}

	a.ActivateCooldown(target, 5*time.Minute)

	if a.ActiveCooldowns() != 1 {
		t.Fatalf("expected 1 active cooldown, got %d", a.ActiveCooldowns())
	}
}

func TestActivateCooldown_ZeroDuration(t *testing.T) {
	a := NewAgentState(testLogger())
	target := UpstreamTarget{
		CoolKey: "agent:gemini:gemini-2.5-flash",
	}

	a.ActivateCooldown(target, 0)

	if a.ActiveCooldowns() != 0 {
		t.Fatalf("expected 0 active cooldowns with zero duration, got %d", a.ActiveCooldowns())
	}
}

func TestActivateCooldown_EmptyCoolKey(t *testing.T) {
	a := NewAgentState(testLogger())
	target := UpstreamTarget{
		CoolKey: "",
	}

	a.ActivateCooldown(target, 5*time.Minute)

	if a.ActiveCooldowns() != 0 {
		t.Fatalf("expected 0 active cooldowns with empty CoolKey, got %d", a.ActiveCooldowns())
	}
}

func TestActivateCooldown_Expires(t *testing.T) {
	a := NewAgentState(testLogger())
	target := UpstreamTarget{
		CoolKey: "agent:gemini:gemini-2.5-flash",
	}

	a.ActivateCooldown(target, 1*time.Nanosecond)
	time.Sleep(5 * time.Millisecond)

	if a.ActiveCooldowns() != 0 {
		t.Fatalf("expected 0 active cooldowns after expiry, got %d", a.ActiveCooldowns())
	}
}

func TestActiveCooldowns_Multiple(t *testing.T) {
	a := NewAgentState(testLogger())

	a.ActivateCooldown(UpstreamTarget{CoolKey: "a"}, 5*time.Minute)
	a.ActivateCooldown(UpstreamTarget{CoolKey: "b"}, 5*time.Minute)
	a.ActivateCooldown(UpstreamTarget{CoolKey: "c"}, 5*time.Minute)

	if got := a.ActiveCooldowns(); got != 3 {
		t.Fatalf("expected 3 active cooldowns, got %d", got)
	}
}

func TestResolveWindowMaxContext_WithModels(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"test-agent": {
			Models: []config.AgentModel{
				{Provider: "nvidia_free", Model: "nemotron-3-super"},
				{Provider: "gemini", Model: "gemini-2.5-flash"},
			},
		},
	}

	got := ResolveWindowMaxContext("test-agent", agents)
	if got != 128000 {
		t.Fatalf("expected 128000 (max of 4000 and 128000), got %d", got)
	}
}

func TestResolveWindowMaxContext_AgentNotFound(t *testing.T) {
	agents := map[string]config.AgentConfig{}

	got := ResolveWindowMaxContext("nonexistent", agents)
	if got != 0 {
		t.Fatalf("expected 0 for nonexistent agent, got %d", got)
	}
}

func TestResolveWindowMaxContext_AgentLevelMaxContext(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"test-agent": {
			Models: []config.AgentModel{
				{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 50000},
				{Provider: "nvidia_free", Model: "nemotron-3-super"},
			},
		},
	}

	got := ResolveWindowMaxContext("test-agent", agents)
	if got != 50000 {
		t.Fatalf("expected 50000 (max of agent-level 50000 and registry 4000), got %d", got)
	}
}
