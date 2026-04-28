package config

import (
	"encoding/json"
	"testing"
)

func TestGovernanceConfig_EmptyStreamAsErrorDefault(t *testing.T) {
	cfg := GovernanceConfig{}
	if cfg.EmptyStreamAsError != false {
		t.Fatalf("expected EmptyStreamAsError to default to false, got %v", cfg.EmptyStreamAsError)
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
}
