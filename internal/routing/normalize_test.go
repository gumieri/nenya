package routing

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/internal/infra"
)

// NENYA-186: harness payload normalization. Duplicated parameters after a
// conversion resolve to one canonical field (client value wins) with
// telemetry, and unknown top-level fields are reported instead of vanishing.

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
			normalizeDuplicateParams(deps, tc.payload)
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
			normalizeDuplicateParams(deps, tc.payload)
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
