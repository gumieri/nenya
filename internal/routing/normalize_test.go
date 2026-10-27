package routing

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// NENYA-186: harness payload normalization. Duplicated parameters after a
// conversion resolve to one canonical field with telemetry — a conflicting
// numeric cap keeps the SMALLER value (neither the client's tighter budget
// nor the model clamp is exceeded) — and unknown top-level fields are
// reported instead of vanishing.

func newNormalizeDeps(t *testing.T) (TransformDeps, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return TransformDeps{
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
		Metrics: infra.NewMetrics(),
	}, &buf
}

func TestNormalizeDuplicateParams(t *testing.T) {
	cases := []struct {
		name      string
		payload   map[string]interface{}
		wantParam string // canonical key
		wantValue interface{}
		wantCount int // duplicate_params metric total
	}{
		{
			name:      "conflicting max tokens keeps the smaller cap",
			payload:   map[string]interface{}{"max_tokens": float64(1024), "max_completion_tokens": float64(4096)},
			wantParam: "max_tokens",
			wantValue: float64(1024),
			wantCount: 1,
		},
		{
			name:      "agreeing duplicates collapse silently",
			payload:   map[string]interface{}{"max_tokens": float64(2048), "max_completion_tokens": float64(2048)},
			wantParam: "max_tokens",
			wantValue: float64(2048),
			wantCount: 0,
		},
		{
			name:      "conflicting stop sequences keeps the canonical spelling",
			payload:   map[string]interface{}{"stop": []interface{}{"a"}, "stop_sequences": []interface{}{"b"}},
			wantParam: "stop",
			wantValue: []interface{}{"a"},
			wantCount: 1,
		},
		{
			name:      "client alias smaller than the gateway clamp wins",
			payload:   map[string]interface{}{"max_tokens": float64(8192), "max_completion_tokens": float64(4096)},
			wantParam: "max_tokens",
			wantValue: float64(4096),
			wantCount: 1,
		},
		{
			name:      "null alias leaves the canonical value intact",
			payload:   map[string]interface{}{"max_tokens": float64(1024), "max_completion_tokens": nil},
			wantParam: "max_tokens",
			wantValue: float64(1024),
			wantCount: 0,
		},
		{
			name:      "single spelling untouched",
			payload:   map[string]interface{}{"max_tokens": float64(10)},
			wantParam: "max_tokens",
			wantValue: float64(10),
			wantCount: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := newNormalizeDeps(t)
			normalizeDuplicateParams(deps, tc.payload, "")
			if got := tc.payload[tc.wantParam]; !equalParamValues(got, tc.wantValue) {
				t.Fatalf("canonical %s = %#v, want %#v", tc.wantParam, got, tc.wantValue)
			}
			alias := map[string]string{"max_tokens": "max_completion_tokens", "stop": "stop_sequences"}[tc.wantParam]
			if _, still := tc.payload[alias]; still {
				t.Fatalf("alias %s survived normalization", alias)
			}
			var sb strings.Builder
			deps.Metrics.WritePrometheus(&sb)
			total := strings.Count(sb.String(), "nenya_duplicate_params_total{")
			if tc.wantCount == 0 && total != 0 {
				t.Fatalf("agreeing duplicates must not report: %s", sb.String())
			}
			if tc.wantCount > 0 {
				if total == 0 {
					t.Fatal("conflicting duplicate must be reported")
				}
				if !strings.Contains(sb.String(), `param="`+tc.wantParam+`"`) {
					t.Fatalf("metric label missing the logical param:\n%s", sb.String())
				}
			}
		})
	}
}

func TestAuditRequestFields(t *testing.T) {
	deps, logBuf := newNormalizeDeps(t)
	payload := map[string]interface{}{
		"model":    "gpt-test",
		"messages": []interface{}{},
		// Harness envelopes the gateway does not consume (the LiteLLM
		// additional_tools class) plus a foreign field.
		"additional_tools": []interface{}{map[string]interface{}{"name": "shell"}},
		"vendor_quirk":     true,
	}
	auditRequestFields(deps, payload, "openai")

	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_unknown_request_fields_total{format="openai"} 1`) {
		t.Fatalf("unknown-field metric missing:\n%s", sb.String())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "additional_tools") || !strings.Contains(logged, "vendor_quirk") {
		t.Fatalf("unknown fields must be named in the log: %s", logged)
	}
	// The field NAME must never become a metric label (client-chosen).
	if strings.Contains(sb.String(), "additional_tools") {
		t.Fatalf("client-chosen field name leaked into metric labels:\n%s", sb.String())
	}
}

func TestAuditRequestFieldsCleanPayloadSilent(t *testing.T) {
	deps, logBuf := newNormalizeDeps(t)
	payload := map[string]interface{}{
		"model": "gpt-test", "messages": []interface{}{},
		"stream": true, "max_tokens": float64(10), "tools": []interface{}{},
		"tool_choice": "auto", "reasoning_effort": "high",
	}
	auditRequestFields(deps, payload, "openai")

	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if strings.Contains(sb.String(), "nenya_unknown_request_fields_total{") {
		t.Fatalf("known fields must not be reported:\n%s", sb.String())
	}
	if strings.Contains(logBuf.String(), "no format path consumes") {
		t.Fatalf("known fields must not log: %s", logBuf.String())
	}
}

// NENYA-186: in-memory numeric types differ (gateway-injected int vs decoded
// float64); the smaller-cap rule must hold across them.
func TestNormalizeDuplicateParamsNumericTypes(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    float64
	}{
		{
			name:    "client alias float64 smaller than int clamp",
			payload: map[string]interface{}{"max_tokens": 8192, "max_completion_tokens": float64(4096)},
			want:    4096,
		},
		{
			name:    "client alias int smaller than float64 clamp",
			payload: map[string]interface{}{"max_tokens": float64(8192), "max_completion_tokens": 4096},
			want:    4096,
		},
		{
			name:    "agreeing int and float64 collapse without conflict",
			payload: map[string]interface{}{"max_tokens": 8192, "max_completion_tokens": float64(8192)},
			want:    8192,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := newNormalizeDeps(t)
			normalizeDuplicateParams(deps, tc.payload, "")
			got, ok := numericValue(tc.payload["max_tokens"])
			if !ok || got != tc.want {
				t.Fatalf("max_tokens = %#v, want %v", tc.payload["max_tokens"], tc.want)
			}
			if _, alias := tc.payload["max_completion_tokens"]; alias {
				t.Fatal("alias survived normalization")
			}
			var sb strings.Builder
			deps.Metrics.WritePrometheus(&sb)
			conflicts := strings.Count(sb.String(), "nenya_duplicate_params_total{")
			if strings.Contains(tc.name, "agreeing") && conflicts != 0 {
				t.Fatalf("agreeing duplicates must not report:\n%s", sb.String())
			}
			if !strings.Contains(tc.name, "agreeing") && conflicts == 0 {
				t.Fatal("conflicting duplicates must report")
			}
		})
	}
}

// NENYA-179: the same client payload dispatched to two different targets must
// yield correctly normalized variants — the mutation is a function of the
// (client format, target executor) pair, not of the client format alone. The
// target axis is operator-declared (no built-in rename: a model-ID prefix is
// not evidence about the executor behind it).
func TestNormalizeTargetParamsRoundTripTwoTargets(t *testing.T) {
	clientPayload := func() map[string]interface{} {
		// A JSON-decoded client body: the cap is float64 under the newer
		// spelling, plus the gateway's canonical slot as a float.
		return map[string]interface{}{
			"model":                 "x",
			"messages":              []interface{}{},
			"max_tokens":            float64(8192),
			"max_completion_tokens": float64(4096),
		}
	}
	strictRules := []paramCompatRule{{
		prefix:         "gpt-5.3-codex",
		tokenField:     "max_completion_tokens",
		intTypedParams: []string{"max_completion_tokens"},
	}}
	// A MATCHING rule without a token_field: the canonical spelling must
	// survive (this pins "declared axis absent", not just "no rule matched").
	tolerantRules := []paramCompatRule{{prefix: "some-tolerant", dropParams: []string{"temperature"}}}

	t.Run("declared strict target gets the required spelling and integer type", func(t *testing.T) {
		deps, _ := newNormalizeDeps(t)
		payload := clientPayload()
		normalizeTargetParams(deps, payload, strictRules, "gpt-5.3-codex", "openai")

		if _, stale := payload["max_tokens"]; stale {
			t.Fatalf("max_tokens must be dropped for a max_completion_tokens target: %#v", payload)
		}
		v, ok := payload["max_completion_tokens"].(int)
		if !ok {
			t.Fatalf("max_completion_tokens must be integer-typed, got %#v", payload["max_completion_tokens"])
		}
		if v != 4096 {
			t.Fatalf("cap = %d, want the smaller client cap 4096", v)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if !strings.Contains(sb.String(), `nenya_param_coercions_total{client_format="openai", param="max_completion_tokens"} 1`) {
			t.Fatalf("coercion must be recorded:\n%s", sb.String())
		}
	})

	t.Run("tolerant target keeps the canonical spelling and client shape", func(t *testing.T) {
		deps, _ := newNormalizeDeps(t)
		payload := clientPayload()
		normalizeTargetParams(deps, payload, tolerantRules, "some-tolerant-model", "openai")

		if _, stale := payload["max_completion_tokens"]; stale {
			t.Fatalf("alias must collapse for a tolerant target: %#v", payload)
		}
		v, ok := payload["max_tokens"].(float64)
		if !ok || v != 4096 {
			t.Fatalf("max_tokens = %#v, want the smaller cap 4096 in the client's shape", payload["max_tokens"])
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if strings.Contains(sb.String(), "nenya_param_coercions_total{") {
			t.Fatalf("a tolerant target must not coerce:\n%s", sb.String())
		}
	})

	t.Run("unmatched model is untouched even with strict rules present", func(t *testing.T) {
		deps, _ := newNormalizeDeps(t)
		payload := map[string]interface{}{"max_tokens": float64(2048)}
		normalizeTargetParams(deps, payload, strictRules, "gpt-4o", "openai")
		if _, moved := payload["max_completion_tokens"]; moved {
			t.Fatalf("an unmatched model must not be renamed: %#v", payload)
		}
		if v, ok := payload["max_tokens"].(float64); !ok || v != 2048 {
			t.Fatalf("cap = %#v, want the client's 2048 float", payload["max_tokens"])
		}
	})
}

// TestNormalizeTargetParamsClientAxis: an Anthropic-native client's converted
// body is also JSON-decoded (float shape), and the coercion records which
// client format it came from.
func TestNormalizeTargetParamsClientAxis(t *testing.T) {
	deps, _ := newNormalizeDeps(t)
	rules := []paramCompatRule{{
		prefix:         "strict-executor",
		tokenField:     "max_completion_tokens",
		intTypedParams: []string{"max_completion_tokens"},
	}}
	payload := map[string]interface{}{"max_tokens": float64(2048)}
	normalizeTargetParams(deps, payload, rules, "strict-executor-1", "anthropic")

	if v, ok := payload["max_completion_tokens"].(int); !ok || v != 2048 {
		t.Fatalf("max_completion_tokens = %#v, want int 2048", payload["max_completion_tokens"])
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `client_format="anthropic"`) {
		t.Fatalf("client axis must be recorded:\n%s", sb.String())
	}
}

// TestNormalizeTargetParamsConfigRuleOverride: an operator-declared rule wins
// over the built-in table (first match), including the token spelling.
func TestNormalizeTargetParamsConfigRuleOverride(t *testing.T) {
	deps, _ := newNormalizeDeps(t)
	deps.Config = &config.Config{Governance: config.GovernanceConfig{
		ParamCompat: []config.ParamCompatRule{
			{ModelPrefix: "gpt-5", TokenField: "max_tokens"},
		},
	}}
	payload := map[string]interface{}{
		"max_tokens":            float64(8192),
		"max_completion_tokens": float64(4096),
	}
	normalizeTargetParams(deps, payload, resolveParamCompatRules(&deps.Config.Governance), "gpt-5.3-codex", "openai")

	if _, stale := payload["max_completion_tokens"]; stale {
		t.Fatalf("operator override must keep max_tokens: %#v", payload)
	}
	if v, ok := numericValue(payload["max_tokens"]); !ok || v != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", payload["max_tokens"])
	}
}

// NENYA-179: the pair-driven rename must not leak to the next failover target
// of a corrective retry (the payload map is shared), and the move is traced at
// the operator-visible level.
func TestTransformRequestForUpstreamCapRenameDoesNotLeak(t *testing.T) {
	strict := config.ParamCompatRule{
		ModelPrefix:    "strict-",
		TokenField:     "max_completion_tokens",
		IntTypedParams: []string{"max_completion_tokens"},
	}
	deps, logBuf := newNormalizeDeps(t)
	deps.Config = &config.Config{Governance: config.GovernanceConfig{ParamCompat: []config.ParamCompatRule{strict}}}

	payload := map[string]interface{}{
		"model":      "strict-executor",
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"max_tokens": float64(2048),
	}

	// Target A (declared strict): the cap rides the required spelling.
	body, model, err := TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "strict-executor", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if model != "strict-executor" {
		t.Fatalf("model = %q", model)
	}
	if !strings.Contains(string(body), `"max_completion_tokens":2048`) || strings.Contains(string(body), `"max_tokens"`) {
		t.Fatalf("strict target body = %s, want the required spelling only", body)
	}
	if !strings.Contains(logBuf.String(), "moved token cap to the target's required field") {
		t.Fatalf("the move must be traced:\n%s", logBuf.String())
	}

	// Target B (tolerant, same shared payload): the original spelling is back.
	body, _, err = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "tolerant-model", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(string(body), `"max_tokens":2048`) {
		t.Fatalf("tolerant target body = %s, want the client's original spelling restored", body)
	}
	if strings.Contains(string(body), `"max_completion_tokens"`) {
		t.Fatalf("the strict target's rename leaked into the shared payload: %s", body)
	}
}

// NENYA-179: the anthropic conversion rebinds the payload map, so the restores
// must run on the ORIGINAL map or the rename leaks into the shared failover
// payload (round-3 review).
func TestTransformRequestForUpstreamCapRenameDoesNotLeakAnthropic(t *testing.T) {
	deps, _ := newNormalizeDeps(t)
	deps.Config = &config.Config{Governance: config.GovernanceConfig{
		ParamCompat: []config.ParamCompatRule{
			{ModelPrefix: "strict-", TokenField: "max_completion_tokens"},
			{ModelPrefix: "tolerant-", TokenField: "max_tokens"},
		},
	}}
	payload := map[string]interface{}{
		"model":      "strict-executor",
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"max_tokens": float64(2048),
	}

	// Target A: anthropic format (conversion rebinds the map).
	if _, _, err := TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "strict-executor", MaxOutput: 0, MaxContext: 0, Format: "anthropic", ReasoningEffort: ""}, payload); err != nil {
		t.Fatalf("transform: %v", err)
	}
	// Target B: same shared payload, tolerant-but-declared canonical spelling.
	body, _, err := TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "tolerant-model", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(string(body), `"max_tokens":2048`) || strings.Contains(string(body), `"max_completion_tokens"`) {
		t.Fatalf("the anthropic dispatch leaked the rename into the shared payload: %s", body)
	}
}
