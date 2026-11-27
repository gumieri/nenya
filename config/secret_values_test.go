package config

import (
	"strings"
	"testing"
	"time"
)

// NENYA-350 (literal-value list via secret refs) config pins: the list
// is bounded, entries must be non-empty and meet the minimum length,
// and env:/file: references resolve fail-closed at load with
// index-named errors.

func TestValidateSecretValuesRejectsShort(t *testing.T) {
	errs := validateSecretValues(&SecretsConfig{SecretValues: []string{"hunter2secret", "abc"}})
	if len(errs) != 1 || !strings.Contains(errs[0], "secret_values[1]") {
		t.Fatalf("expected exactly the short-entry error, got %v", errs)
	}
}

// TestValidateSecretValuesRawPassExemptsRefs pins the two-pass gate
// (round-1 MED): on the RAW document a short-named env:/file: REFERENCE
// is exempt from the length check (a short ref name is not a short
// secret) — the RESOLVED values are length-gated post-resolution in
// validateSecretsResult.
func TestValidateSecretValuesRawPassExemptsRefs(t *testing.T) {
	errs := validateSecretValues(&SecretsConfig{SecretValues: []string{"env:PW", "file:s"}})
	if len(errs) != 0 {
		t.Fatalf("raw pass length-gated references: %v", errs)
	}
}

// TestValidateSecretsResultGatesResolvedValues pins the post-resolution
// gate: a reference RESOLVED to a below-minimum value refuses the load
// (the actually-serving value is length-gated, not the ref name). The
// pin mirrors LoadSecretsBounded's sequence: resolve, then validate.
func TestValidateSecretsResultGatesResolvedValues(t *testing.T) {
	t.Setenv("NENYA_TEST_SHORT_SECRET", "ab")
	secrets := &SecretsConfig{
		ClientToken:  "0123456789abcdef",
		SecretValues: []string{"env:NENYA_TEST_SHORT_SECRET"},
	}
	if err := ResolveSecretValueReferences(secrets, "", time.Second); err != nil {
		t.Fatalf("resolution failed: %v", err)
	}
	_, err := validateSecretsResult(secrets, "test")
	if err == nil || !strings.Contains(err.Error(), "secret_values invalid") {
		t.Fatalf("resolved short value not gated at load: %v", err)
	}
}

func TestValidateSecretValuesRejectsEmpty(t *testing.T) {
	errs := validateSecretValues(&SecretsConfig{SecretValues: []string{"          "}})
	if len(errs) != 1 || !strings.Contains(errs[0], "secret_values[0]") {
		t.Fatalf("expected the empty-entry error, got %v", errs)
	}
}

func TestValidateSecretValuesRejectsOverflow(t *testing.T) {
	values := make([]string, MaxSecretValues+1)
	for i := range values {
		values[i] = "0123456789abcdef"
	}
	errs := validateSecretValues(&SecretsConfig{SecretValues: values})
	if len(errs) != 1 || !strings.Contains(errs[0], "cap") {
		t.Fatalf("expected the cap error, got %v", errs)
	}
}

func TestValidateSecretValuesAcceptsGood(t *testing.T) {
	if errs := validateSecretValues(&SecretsConfig{SecretValues: []string{"hunter2secret"}}); len(errs) != 0 {
		t.Fatalf("good list rejected: %v", errs)
	}
	if errs := validateSecretValues(nil); len(errs) != 0 {
		t.Fatalf("nil secrets rejected: %v", errs)
	}
}

func TestResolveSecretValueReferences(t *testing.T) {
	t.Setenv("NENYA_TEST_SECRET_VALUE", "hunter2secret-value")
	secrets := &SecretsConfig{SecretValues: []string{"env:NENYA_TEST_SECRET_VALUE", "plain-literal-value", "file:does-not-exist.txt"}}
	err := ResolveSecretValueReferences(secrets, "", time.Second)
	if err == nil {
		t.Fatal("missing file: reference must fail the load")
	}
	if !strings.Contains(err.Error(), "secret_values[2]") {
		t.Fatalf("error must name the index, got: %v", err)
	}
	// The resolvable entries resolved in place before the failure.
	if secrets.SecretValues[0] != "hunter2secret-value" {
		t.Fatalf("env reference not resolved: %q", secrets.SecretValues[0])
	}
	if secrets.SecretValues[1] != "plain-literal-value" {
		t.Fatalf("inline literal rewritten: %q", secrets.SecretValues[1])
	}
}

func TestResolveSecretValueReferencesNilSafe(t *testing.T) {
	if err := ResolveSecretValueReferences(nil, "", time.Second); err != nil {
		t.Fatalf("nil secrets errored: %v", err)
	}
}

// NENYA-351 (outbound invisible-text guard) config pins.
func TestValidateInvisibleTextConfig(t *testing.T) {
	if errs := validateInvisibleTextConfig(&Config{Governance: GovernanceConfig{InvisibleText: &InvisibleTextConfig{Action: "block"}}}); len(errs) != 1 {
		t.Fatalf("bad action accepted: %v", errs)
	}
	if errs := validateInvisibleTextConfig(&Config{Governance: GovernanceConfig{InvisibleText: &InvisibleTextConfig{Set: "wide"}}}); len(errs) != 1 {
		t.Fatalf("bad set accepted: %v", errs)
	}
	if errs := validateInvisibleTextConfig(&Config{Governance: GovernanceConfig{InvisibleText: &InvisibleTextConfig{Action: "strip", Set: "extended"}}}); len(errs) != 0 {
		t.Fatalf("good config rejected: %v", errs)
	}
	if errs := validateInvisibleTextConfig(&Config{}); len(errs) != 0 {
		t.Fatalf("nil knob rejected: %v", errs)
	}
}

func TestEffectiveInvisibleTextDefaults(t *testing.T) {
	var g GovernanceConfig
	if g.EffectiveInvisibleTextEnabled() {
		t.Fatal("default must be OFF")
	}
	if g.EffectiveInvisibleTextAction() != InvisibleActionLog || g.EffectiveInvisibleTextSet() != InvisibleSetNarrow {
		t.Fatalf("defaults: action %q set %q", g.EffectiveInvisibleTextAction(), g.EffectiveInvisibleTextSet())
	}
	enabled := true
	g.InvisibleText = &InvisibleTextConfig{Enabled: &enabled, Action: "STRIP", Set: "Extended"}
	if !g.EffectiveInvisibleTextEnabled() || g.EffectiveInvisibleTextAction() != "strip" || g.EffectiveInvisibleTextSet() != "extended" {
		t.Fatal("explicit config not honored")
	}
}
