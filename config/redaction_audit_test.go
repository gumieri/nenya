package config

import (
	"strings"
	"testing"
)

// NENYA-362 pins: the redaction audit log config validation — enabling
// requires an explicit path (the writer refuses to guess a location for
// a tamper-evidence artifact) and the default is OFF with no path.
func TestValidateRedactionAudit(t *testing.T) {
	on := true
	off := false
	tests := []struct {
		name    string
		audit   *RedactionAuditConfig
		wantErr string
	}{
		{"nil section is valid", nil, ""},
		{"disabled with no path is valid", &RedactionAuditConfig{Enabled: &off}, ""},
		{"enabled with path is valid", &RedactionAuditConfig{Enabled: &on, Path: "/var/lib/nenya/redaction-audit.jsonl"}, ""},
		{"enabled without path rejected", &RedactionAuditConfig{Enabled: &on}, "governance.redaction_audit.path: must be set when enabled"},
		{"enabled with blank path rejected", &RedactionAuditConfig{Enabled: &on, Path: "   "}, "governance.redaction_audit.path: must be set when enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Governance: GovernanceConfig{RedactionAudit: tt.audit}}
			errs := validateRedactionAuditConfig(cfg)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tt.wantErr) {
				t.Fatalf("errs = %v, want one containing %q", errs, tt.wantErr)
			}
		})
	}
}

func TestEffectiveRedactionAuditDefaults(t *testing.T) {
	var g GovernanceConfig
	if g.EffectiveRedactionAuditEnabled() {
		t.Fatal("default must be OFF")
	}
	if g.EffectiveRedactionAuditPath() != "" {
		t.Fatal("default path must be empty")
	}
}
