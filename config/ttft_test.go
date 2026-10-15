package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTTFTIncludeReasoningFrames pins the NENYA-178 opt-in: reasoning-only
// frames are excluded from the time-to-first-token measurement by default (a
// slow-thinking model must not look slow-to-first-token), and included when the
// operator sets governance.ttft_include_reasoning.
func TestTTFTIncludeReasoningFrames(t *testing.T) {
	if (&GovernanceConfig{}).TTFTIncludeReasoningFrames() {
		t.Error("default must exclude reasoning frames from TTFT")
	}
	if !(&GovernanceConfig{TTFTIncludeReasoning: true}).TTFTIncludeReasoningFrames() {
		t.Error("an explicit opt-in must include reasoning frames")
	}
	var nilCfg *GovernanceConfig
	if nilCfg.TTFTIncludeReasoningFrames() {
		t.Error("a nil governance config must default to excluding reasoning frames")
	}
}

// TestTTFTIncludeReasoningJSONTag pins the wire name of the NENYA-178 opt-in.
func TestTTFTIncludeReasoningJSONTag(t *testing.T) {
	var cfg GovernanceConfig
	if err := json.Unmarshal([]byte(`{"ttft_include_reasoning":true}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.TTFTIncludeReasoningFrames() {
		t.Fatal("ttft_include_reasoning was not decoded from JSON")
	}
	out, err := json.Marshal(GovernanceConfig{TTFTIncludeReasoning: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"ttft_include_reasoning":true`) {
		t.Fatalf("marshaled config lacks the field: %s", out)
	}
}
