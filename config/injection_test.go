package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyDefaults_Injection(t *testing.T) {
	raw := `{"governance": {"injection": {}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.Injection == nil {
		t.Fatal("injection block missing")
	}
	if cfg.Governance.Injection.Enabled == nil || *cfg.Governance.Injection.Enabled {
		t.Error("injection should default to disabled")
	}
	if cfg.Governance.Injection.Strict == nil || *cfg.Governance.Injection.Strict {
		t.Error("injection should default to warn+sanitize (strict off)")
	}
}

func TestApplyDefaults_InjectionExplicitOverrides(t *testing.T) {
	raw := `{"governance": {"injection": {"enabled": true, "strict": true}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.Injection.Enabled == nil || !*cfg.Governance.Injection.Enabled {
		t.Error("explicit enabled=true must survive defaults")
	}
	if cfg.Governance.Injection.Strict == nil || !*cfg.Governance.Injection.Strict {
		t.Error("explicit strict=true must survive defaults")
	}
}

func TestValidateInjectionConfig_RejectsPerAgentPatterns(t *testing.T) {
	raw := `{
		"governance": {"injection": {"enabled": true}},
		"agents": {
			"trusted": {"injection": {"enabled": true, "extra_patterns": ["(?i)custom-hit"]}},
			"strict-agent": {"injection": {"enabled": true, "ignore_patterns": ["(?i)^allow"]}}
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	errs := validateInjectionConfig(&cfg, testLogger())
	if len(errs) != 2 {
		t.Fatalf("expected 2 validation errors for per-agent extra/ignore patterns, got %v", errs)
	}
	found := strings.Join(errs, "\n")
	if !strings.Contains(found, "trusted") || !strings.Contains(found, "extra_patterns") {
		t.Errorf("expected extra_patterns rejection for trusted, got %v", errs)
	}
	if !strings.Contains(found, "strict-agent") || !strings.Contains(found, "ignore_patterns") {
		t.Errorf("expected ignore_patterns rejection for strict-agent, got %v", errs)
	}
}

func TestValidateInjectionConfig_ValidPatterns(t *testing.T) {
	raw := `{"governance": {"injection": {"enabled": true, "extra_patterns": ["(?i)custom-hit"], "ignore_patterns": ["(?i)^security corpus:"]}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if errs := validateInjectionConfig(&cfg, testLogger()); len(errs) != 0 {
		t.Fatalf("expected no validation errors, got %v", errs)
	}
}
