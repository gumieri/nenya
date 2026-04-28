package config

import (
	"encoding/json"
	"testing"
)

func TestGovernanceConfig_EmptyStreamAsErrorRawZeroValue(t *testing.T) {
	cfg := GovernanceConfig{}
	if cfg.EmptyStreamAsError != false {
		t.Fatalf("expected raw EmptyStreamAsError to be false, got %v", cfg.EmptyStreamAsError)
	}
	if cfg.EmptyStreamAsErrorSet() {
		t.Fatal("expected EmptyStreamAsErrorSet to be false for raw zero value")
	}
}

func TestGovernanceConfig_EmptyStreamAsErrorAppliedDefault(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)
	if cfg.Governance.EmptyStreamAsError != true {
		t.Fatalf("expected EmptyStreamAsError to default to true after ApplyDefaults, got %v", cfg.Governance.EmptyStreamAsError)
	}
}

func TestGovernanceConfig_EmptyStreamAsErrorUnmarshal(t *testing.T) {
	jsonData := `{
		"empty_stream_as_error": true
	}`
	var cfg GovernanceConfig
	if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if cfg.EmptyStreamAsError != true {
		t.Fatalf("expected EmptyStreamAsError=true, got %v", cfg.EmptyStreamAsError)
	}
	if !cfg.EmptyStreamAsErrorSet() {
		t.Fatal("expected EmptyStreamAsErrorSet() to be true when set in JSON")
	}
}

func TestGovernanceConfig_EmptyStreamAsErrorUnmarshalFalse(t *testing.T) {
	jsonData := `{
		"empty_stream_as_error": false
	}`
	var cfg GovernanceConfig
	if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if cfg.EmptyStreamAsError != false {
		t.Fatalf("expected EmptyStreamAsError=false, got %v", cfg.EmptyStreamAsError)
	}
	if !cfg.EmptyStreamAsErrorSet() {
		t.Fatal("expected EmptyStreamAsErrorSet() to be true when explicitly set to false")
	}
}

func TestGovernanceConfig_EmptyStreamAsErrorUnmarshalOmitted(t *testing.T) {
	jsonData := `{}`
	var cfg GovernanceConfig
	if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if cfg.EmptyStreamAsError != false {
		t.Fatalf("expected EmptyStreamAsError=false when omitted, got %v", cfg.EmptyStreamAsError)
	}
	if cfg.EmptyStreamAsErrorSet() {
		t.Fatal("expected EmptyStreamAsErrorSet() to be false when omitted")
	}
}

func TestGovernanceConfig_EmptyStreamAsErrorExplicitFalsePreserved(t *testing.T) {
	jsonData := `{
		"governance": { "empty_stream_as_error": false }
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatalf("apply defaults error: %v", err)
	}
	if cfg.Governance.EmptyStreamAsError != false {
		t.Fatalf("expected explicit EmptyStreamAsError=false to be preserved after ApplyDefaults, got %v", cfg.Governance.EmptyStreamAsError)
	}
}
