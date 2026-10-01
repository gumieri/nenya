package config

import (
	"strings"
	"testing"
)

func TestValidateJudgmentsConfig(t *testing.T) {
	t.Run("empty map is valid", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = nil
		if errs := validateJudgmentsConfig(cfg); len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
	})

	t.Run("valid budgets", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{
			"probe": {MaxBytes: 1024, TimeoutSeconds: 5},
		}
		if errs := validateJudgmentsConfig(cfg); len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
	})

	t.Run("valid action accepted", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{
			"probe": {Action: "strict"},
		}
		if errs := validateJudgmentsConfig(cfg); len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
	})

	t.Run("invalid action rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{
			"probe": {Action: "nuke"},
		}
		errs := validateJudgmentsConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "invalid value") {
			t.Errorf("errs = %v, want invalid-action error", errs)
		}
	})

	t.Run("nil entry rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{"probe": nil}
		errs := validateJudgmentsConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "must not be null") {
			t.Errorf("errs = %v, want null-entry error", errs)
		}
	})

	t.Run("bad name rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{"Bad-Name!": {}}
		errs := validateJudgmentsConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "name must match") {
			t.Errorf("errs = %v, want name-pattern error", errs)
		}
	})

	t.Run("empty engine object rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{
			"probe": {Engine: &EngineRef{}},
		}
		errs := validateJudgmentsConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "empty engine reference") {
			t.Errorf("errs = %v, want empty-engine error", errs)
		}
	})

	t.Run("negative budgets rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{
			"probe": {MaxBytes: -1, TimeoutSeconds: -2},
		}
		errs := validateJudgmentsConfig(cfg)
		if len(errs) != 2 {
			t.Fatalf("errs = %v, want 2 errors", errs)
		}
		if !strings.Contains(errs[0], "max_bytes") || !strings.Contains(errs[1], "timeout_seconds") {
			t.Errorf("errs = %v, want max_bytes and timeout_seconds errors", errs)
		}
	})
}

// TestApplyJudgmentDefaults covers the engine inheritance chain:
// explicit engine wins, then the injection-escalation engine, then the
// bouncer engine.
func TestApplyJudgmentDefaults(t *testing.T) {
	bouncerOnly := func(t *testing.T) *Config {
		t.Helper()
		cfg := &Config{}
		cfg.Governance.Judgments = map[string]*JudgmentConfig{"probe": {}}
		return cfg
	}

	t.Run("no escalation falls back to bouncer", func(t *testing.T) {
		cfg := bouncerOnly(t)
		applyJudgmentDefaults(cfg)
		got := cfg.Governance.Judgments["probe"].Engine
		if got != &cfg.Bouncer.Engine {
			t.Fatalf("engine = %+v, want shared bouncer engine pointer", got)
		}
	})

	t.Run("escalation engine wins over bouncer", func(t *testing.T) {
		cfg := bouncerOnly(t)
		escEngine := &EngineRef{Provider: "esc", Model: "classifier"}
		cfg.Governance.Injection = &InjectionConfig{
			Escalation: &InjectionEscalationConfig{Enabled: PtrTo(true), Engine: escEngine},
		}
		applyJudgmentDefaults(cfg)
		got := cfg.Governance.Judgments["probe"].Engine
		if got != escEngine {
			t.Fatalf("engine = %+v, want escalation engine", got)
		}
	})

	t.Run("explicit engine preserved", func(t *testing.T) {
		cfg := bouncerOnly(t)
		explicit := &EngineRef{Provider: "mine", Model: "m"}
		cfg.Governance.Judgments["probe"].Engine = explicit
		applyJudgmentDefaults(cfg)
		if cfg.Governance.Judgments["probe"].Engine != explicit {
			t.Fatal("explicit engine was overwritten")
		}
	})

	t.Run("shared pointer across entries", func(t *testing.T) {
		cfg := bouncerOnly(t)
		cfg.Governance.Judgments["second"] = &JudgmentConfig{}
		applyJudgmentDefaults(cfg)
		if cfg.Governance.Judgments["probe"].Engine != cfg.Governance.Judgments["second"].Engine {
			t.Fatal("inheriting entries must share one engine pointer")
		}
	})
}

func TestResolveEngineRefsJudgments(t *testing.T) {
	cfg := &Config{}
	cfg.Providers = map[string]ProviderConfig{
		"stub": {URL: "http://127.0.0.1:9/v1", ApiFormat: "openai"},
	}
	cfg.Governance.Judgments = map[string]*JudgmentConfig{
		"probe": {Engine: &EngineRef{Provider: "stub", Model: "judge"}},
	}
	if err := resolveEngineRefs(cfg); err != nil {
		t.Fatalf("resolveEngineRefs: %v", err)
	}
	targets := cfg.Governance.Judgments["probe"].Engine.ResolvedTargets
	if len(targets) != 1 {
		t.Fatalf("resolved targets = %d, want 1", len(targets))
	}
	if targets[0].Provider.Name != "stub" || targets[0].Engine.Model != "judge" {
		t.Errorf("unexpected target: %+v", targets[0])
	}
}
