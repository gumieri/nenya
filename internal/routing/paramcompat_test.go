package routing

import (
	"testing"

	"github.com/nenya/config"
)

func TestApplyParamCompat_Gemini3DropsSamplingParams(t *testing.T) {
	payload := map[string]interface{}{
		"temperature":     0.7,
		"top_p":           0.9,
		"top_k":           40,
		"candidate_count": 2,
		"max_tokens":      1024,
		"messages":        []interface{}{},
	}
	applied := applyParamCompat(builtInParamCompat, payload, "gemini-3-pro")

	for _, p := range []string{"temperature", "top_p", "top_k", "candidate_count"} {
		if _, ok := payload[p]; ok {
			t.Errorf("expected %q to be dropped for gemini-3", p)
		}
	}
	if payload["max_tokens"] != 1024 {
		t.Error("unrelated params must stay untouched")
	}
	if len(applied) != 4 {
		t.Errorf("expected 4 applied params, got %v", applied)
	}
}

func TestApplyParamCompat_ProviderQualifiedModel(t *testing.T) {
	payload := map[string]interface{}{"temperature": 0.5}
	applyParamCompat(builtInParamCompat, payload, "google/gemini-3-flash")
	if _, ok := payload["temperature"]; ok {
		t.Error("provider-qualified gemini-3 IDs must match the rule")
	}
}

func TestApplyParamCompat_NoRuleNoChange(t *testing.T) {
	payload := map[string]interface{}{"temperature": 0.5, "candidate_count": 1}
	if applied := applyParamCompat(builtInParamCompat, payload, "gpt-4o"); applied != nil {
		t.Errorf("expected no rule to match gpt-4o, got %v", applied)
	}
	if payload["temperature"] != 0.5 || payload["candidate_count"] != 1 {
		t.Error("payload must be untouched when no rule matches")
	}
}

func TestApplyParamCompat_ConfigOverrideAndPrecedence(t *testing.T) {
	gov := &config.GovernanceConfig{
		ParamCompat: []config.ParamCompatRule{
			{ModelPrefix: "my-model", DropParams: []string{"weird_param"}, MinReasoningEffort: "low"},
			// Deliberately empty: first-match-wins means this shadows the
			// built-in gemini-3 drops for operators who disagree with them.
			{ModelPrefix: "gemini-3"},
		},
	}
	rules := resolveParamCompatRules(gov)

	// Config rule applies with all its actions.
	payload := map[string]interface{}{
		"weird_param":      true,
		"reasoning_effort": "minimal",
	}
	applied := applyParamCompat(rules, payload, "my-model-v2")
	if _, ok := payload["weird_param"]; ok {
		t.Error("config drop_params must be applied")
	}
	if payload["reasoning_effort"] != "low" {
		t.Errorf("config min_reasoning_effort must clamp, got %v", payload["reasoning_effort"])
	}
	if len(applied) != 2 {
		t.Errorf("expected weird_param+reasoning_effort applied, got %v", applied)
	}

	// Precedence: the empty config rule shadows the built-in drops for
	// every gemini-3 ID — first-match-wins across config+built-in.
	shadowed := map[string]interface{}{"temperature": 0.5}
	if applied := applyParamCompat(rules, shadowed, "gemini-3-pro"); applied != nil {
		t.Errorf("config rule must take precedence over built-in drops, got %v", applied)
	}
	if shadowed["temperature"] != 0.5 {
		t.Error("shadowed rule must not drop params")
	}

	// Built-in table still reachable for prefixes config does not shadow
	// (here: a config rule for one family, built-in gemini-3 for another).
	gov2 := &config.GovernanceConfig{
		ParamCompat: []config.ParamCompatRule{
			{ModelPrefix: "other-model", DropParams: []string{"other_param"}},
		},
	}
	gem := map[string]interface{}{"temperature": 0.5}
	applyParamCompat(resolveParamCompatRules(gov2), gem, "gemini-3-pro")
	if _, ok := gem["temperature"]; ok {
		t.Error("built-in gemini-3 rule must apply when config shadows only other prefixes")
	}
}

func TestApplyParamCompat_ClampReasoningEffort(t *testing.T) {
	rules := []paramCompatRule{{prefix: "clamp-model", minReasoningEffort: "low"}}

	clamped := map[string]interface{}{"reasoning_effort": "minimal"}
	if applyParamCompat(rules, clamped, "clamp-model") == nil {
		t.Fatal("expected clamp to fire")
	}
	if clamped["reasoning_effort"] != "low" {
		t.Errorf("expected minimal clamped to low, got %v", clamped["reasoning_effort"])
	}

	for _, effort := range []string{"low", "medium", "high"} {
		stays := map[string]interface{}{"reasoning_effort": effort}
		applyParamCompat(rules, stays, "clamp-model")
		if stays["reasoning_effort"] != effort {
			t.Errorf("effort %q at or above floor must stay, got %v", effort, stays["reasoning_effort"])
		}
	}

	unknown := map[string]interface{}{"reasoning_effort": "turbo"}
	applyParamCompat(rules, unknown, "clamp-model")
	if unknown["reasoning_effort"] != "turbo" {
		t.Error("unknown effort values must never be clamped")
	}

	absent := map[string]interface{}{}
	applyParamCompat(rules, absent, "clamp-model")
	if _, ok := absent["reasoning_effort"]; ok {
		t.Error("clamp must never inject a reasoning_effort")
	}
}

func TestApplyParamCompat_RelaxForcedToolChoice(t *testing.T) {
	rules := []paramCompatRule{{prefix: "relax-model", relaxForcedToolChoice: true}}

	forced := map[string]interface{}{"tool_choice": "required"}
	applyParamCompat(rules, forced, "relax-model")
	if forced["tool_choice"] != "auto" {
		t.Errorf("expected required relaxed to auto, got %v", forced["tool_choice"])
	}

	namedFn := map[string]interface{}{
		"tool_choice": map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f"}},
	}
	applyParamCompat(rules, namedFn, "relax-model")
	if namedFn["tool_choice"] != "auto" {
		t.Errorf("expected forced function relaxed to auto, got %v", namedFn["tool_choice"])
	}

	for _, keep := range []interface{}{"auto", "none"} {
		stays := map[string]interface{}{"tool_choice": keep}
		applyParamCompat(rules, stays, "relax-model")
		if stays["tool_choice"] != keep {
			t.Errorf("tool_choice %v must stay untouched", keep)
		}
	}
	allowed := map[string]interface{}{"tool_choice": map[string]interface{}{"type": "allowed_tools"}}
	applyParamCompat(rules, allowed, "relax-model")
	typ := allowed["tool_choice"].(map[string]interface{})["type"]
	if typ != "allowed_tools" {
		t.Errorf("allowed_tools tool_choice must stay untouched, got %v", allowed["tool_choice"])
	}
}

func TestResolveParamCompatRules_NilConfigUsesBuiltIn(t *testing.T) {
	rules := resolveParamCompatRules(nil)
	if len(rules) != len(builtInParamCompat) {
		t.Fatalf("expected built-in rules only, got %d", len(rules))
	}
}

func TestStripParams(t *testing.T) {
	payload := map[string]interface{}{"temperature": 1, "top_p": 2, "messages": "x"}
	StripParams(payload, []string{"temperature", "top_p", "absent_param"})
	if len(payload) != 1 {
		t.Errorf("expected only messages to remain, got %v", payload)
	}
}
