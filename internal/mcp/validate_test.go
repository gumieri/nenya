package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func guard(enabled bool) ArgGuardConfig {
	return ArgGuardConfig{Enabled: enabled, MaxArgBytes: DefaultMaxArgBytes, URLPolicy: URLPolicyDenyPrivate}
}

func schemaFromJSON(t *testing.T, raw string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	return out
}

func TestValidateArgsDisabled(t *testing.T) {
	cfg := guard(false)
	if v, rejected := ValidateArgs(map[string]any{"url": "http://169.254.169.254/"}, nil, cfg); v != nil || rejected {
		t.Errorf("disabled guard returned %v rejected=%v", v, rejected)
	}
}

func TestValidateArgsHappyPath(t *testing.T) {
	schema := schemaFromJSON(t, `{"type":"object","required":["q"],"properties":{"q":{"type":"string"},"n":{"type":"integer"}}}`)
	args := map[string]any{"q": "golang proxy", "n": float64(3)}
	v, rejected := ValidateArgs(args, schema, guard(true))
	if rejected || len(v) != 0 {
		t.Errorf("happy path flagged %v rejected=%v", v, rejected)
	}
}

func TestValidateArgsSchemaViolations(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		args   map[string]any
		want   string
	}{
		{
			name:   "missing required",
			schema: `{"type":"object","required":["q"]}`,
			args:   map[string]any{"other": "x"},
			want:   "missing required property q",
		},
		{
			name:   "type mismatch",
			schema: `{"type":"object","properties":{"n":{"type":"string"}}}`,
			args:   map[string]any{"n": float64(7)},
			want:   "expected type string",
		},
		{
			name:   "enum violation",
			schema: `{"type":"object","properties":{"mode":{"enum":["fast","slow"]}}}`,
			args:   map[string]any{"mode": "sideways"},
			want:   "enum",
		},
		{
			name:   "nested array items",
			schema: `{"type":"object","properties":{"hosts":{"items":{"type":"string"}}}}`,
			args:   map[string]any{"hosts": []any{"ok", float64(1)}},
			want:   "expected type string",
		},
		{
			name:   "integer accepts whole floats",
			schema: `{"type":"object","properties":{"n":{"type":"integer"}}}`,
			args:   map[string]any{"n": float64(4)},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, rejected := ValidateArgs(tt.args, schemaFromJSON(t, tt.schema), guard(true))
			if tt.want == "" {
				if rejected {
					t.Errorf("unexpected rejection: %v", v)
				}
				return
			}
			if !rejected {
				t.Fatalf("expected rejection, got %v", v)
			}
			if !strings.Contains(v[0].Reason, tt.want) {
				t.Errorf("reason %q, want substring %q", v[0].Reason, tt.want)
			}
		})
	}
}

func TestValidateArgsUnknownKeywordsIgnored(t *testing.T) {
	// format/patternProperties/minLength unsupported: must not reject.
	schema := schemaFromJSON(t, `{"type":"object","properties":{"email":{"type":"string","format":"email","minLength":100}}}`)
	v, rejected := ValidateArgs(map[string]any{"email": "x"}, schema, guard(true))
	if rejected || len(v) != 0 {
		t.Errorf("unknown keywords flagged: %v rejected=%v", v, rejected)
	}
}

func TestValidateArgsDepthCap(t *testing.T) {
	// Recursion is value-driven: build a matching value+schema chain
	// nested deeper than maxSchemaDepth.
	deepValue := any(map[string]any{})
	for i := 0; i < maxSchemaDepth+5; i++ {
		deepValue = map[string]any{"n": deepValue}
	}
	schema := map[string]any{"type": "object"}
	node := schema
	for i := 0; i < maxSchemaDepth+5; i++ {
		inner := map[string]any{"type": "object"}
		node["properties"] = map[string]any{"n": inner}
		node = inner
	}
	v, rejected := ValidateArgs(deepValue.(map[string]any), any(schema), guard(true))
	if !rejected {
		t.Fatal("deep nesting must be capped")
	}
	if !strings.Contains(v[0].Reason, "cap") {
		t.Errorf("reason %q missing cap mention", v[0].Reason)
	}
}

func TestValidateArgsSizeCap(t *testing.T) {
	cfg := guard(true)
	cfg.MaxArgBytes = 32
	v, rejected := ValidateArgs(map[string]any{"blob": strings.Repeat("x", 64)}, nil, cfg)
	if !rejected || len(v) == 0 || !strings.Contains(v[0].Reason, "byte limit") {
		t.Errorf("size cap not enforced: %v rejected=%v", v, rejected)
	}
}

func TestValidateArgsURLPolicy(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		allowed  []string
		policy   string
		rejected bool
		flagged  bool
	}{
		{name: "metadata IP", url: "http://169.254.169.254/latest/meta-data", rejected: true, flagged: true},
		{name: "metadata hostname", url: "http://metadata.google.internal/computeMetadata/v1/", rejected: true, flagged: true},
		{name: "loopback", url: "http://127.0.0.1:8080/admin", rejected: true, flagged: true},
		{name: "ipv6 loopback", url: "http://[::1]:9000/", rejected: true, flagged: true},
		{name: "v4-mapped ipv6", url: "http://[::ffff:127.0.0.1]/", rejected: true, flagged: true},
		{name: "localhost name", url: "http://localhost/x", rejected: true, flagged: true},
		{name: "rfc1918", url: "https://10.1.2.3/internal", rejected: true, flagged: true},
		{name: "172.16 range", url: "https://172.16.0.9/", rejected: true, flagged: true},
		{name: "192.168 range", url: "https://192.168.1.1/", rejected: true, flagged: true},
		{name: "internal suffix", url: "https://db.internal/query", rejected: true, flagged: true},
		{name: "local suffix", url: "https://printer.local/", rejected: true, flagged: true},
		{name: "uppercase scheme", url: "HTTP://127.0.0.1:8080/", rejected: true, flagged: true},
		{name: "decimal ipv4", url: "http://2130706433/", rejected: true, flagged: true},
		{name: "hex ipv4", url: "http://0x7f000001/", rejected: true, flagged: true},
		{name: "octal ipv4", url: "http://0177.0.0.1/", rejected: true, flagged: true},
		{name: "short ipv4", url: "http://127.1/", rejected: true, flagged: true},
		{name: "metadata as decimal", url: "http://2852039166/", rejected: true, flagged: true},
		{name: "ipv6 zone", url: "http://[fe80::1%25eth0]/", rejected: true, flagged: true},
		{name: "trailing dots", url: "http://localhost../", rejected: true, flagged: true},
		{name: "ipv4-compatible ipv6", url: "http://[::7f00:1]/", rejected: true, flagged: true},
		{name: "nat64", url: "http://[64:ff9b::7f00:1]/", rejected: true, flagged: true},
		{name: "broadcast", url: "http://255.255.255.255/", rejected: true, flagged: true},
		{name: "leading whitespace bypass", url: " http://127.0.0.1/", rejected: true, flagged: true},
		{name: "url then prose public", url: "https://example.com is the docs", flagged: false},
		{name: "url then prose private", url: "http://127.0.0.1/ is the admin", rejected: true, flagged: true},
		{name: "malformed then prose", url: "http://[::1/path broken thing", rejected: true, flagged: true},
		{name: "host-less malformed", url: "http://", rejected: true, flagged: true},
		{name: "unterminated ipv6", url: "http://[::1", rejected: true, flagged: true},
		{name: "userinfo only", url: "http://user:pass@", rejected: true, flagged: true},
		{name: "percent-encoded host", url: "http://%31%32%37.0.0.1/", rejected: true, flagged: true},
		{name: "public ok", url: "https://example.com/search", flagged: false},
		{name: "non-url string ok", url: "just a search query", flagged: false},
		{name: "ftp scheme ignored", url: "ftp://127.0.0.1/file", flagged: false},
		{
			name:     "allowlist exact",
			url:      "http://10.0.0.5:8080/api",
			allowed:  []string{"10.0.0.5"},
			rejected: false, flagged: false,
		},
		{
			name:     "allowlist wildcard",
			url:      "http://api.internal/v1",
			allowed:  []string{"*.internal"},
			rejected: false, flagged: false,
		},
		{
			name:     "allowlist does not cover others",
			url:      "http://169.254.169.254/",
			allowed:  []string{"*.internal"},
			rejected: true, flagged: true,
		},
		{
			name:     "log policy flags but allows",
			url:      "http://127.0.0.1/",
			policy:   URLPolicyLog,
			rejected: false, flagged: true,
		},
		{
			name:     "off policy ignores",
			url:      "http://127.0.0.1/",
			policy:   URLPolicyOff,
			rejected: false, flagged: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := guard(true)
			cfg.AllowedHosts = tt.allowed
			if tt.policy != "" {
				cfg.URLPolicy = tt.policy
			}
			v, rejected := ValidateArgs(map[string]any{"url": tt.url}, nil, cfg)
			if rejected != tt.rejected {
				t.Errorf("rejected=%v (%v), want %v", rejected, v, tt.rejected)
			}
			if flagged := len(v) > 0; flagged != tt.flagged {
				t.Errorf("flags=%d, want flagged=%v", len(v), tt.flagged)
			}
		})
	}
}

func TestValidateArgsNestedURLs(t *testing.T) {
	args := map[string]any{
		"targets": []any{
			map[string]any{"host": "https://public.example/ok"},
			map[string]any{"host": "http://[::1]/evil"},
		},
	}
	v, rejected := ValidateArgs(args, nil, guard(true))
	if !rejected {
		t.Fatal("nested private URL must reject")
	}
	if v[0].Path != "targets[1].host" {
		t.Errorf("path = %q, want targets[1].host", v[0].Path)
	}
}

func TestSummarizeArgViolations(t *testing.T) {
	many := make([]ArgViolation, 10)
	summary := SummarizeArgViolations(many)
	if !strings.Contains(summary, "and 5 more") {
		t.Errorf("summary missing overflow note: %q", summary)
	}
	huge := SummarizeArgViolations([]ArgViolation{{Path: "p", Reason: strings.Repeat("r", 1000)}})
	if len(huge) > 512 {
		t.Errorf("summary not capped: %d", len(huge))
	}
	if !utf8.ValidString(huge) {
		t.Error("truncation split a UTF-8 rune")
	}
}

func TestSummarizeArgViolationsRuneBoundary(t *testing.T) {
	// Paths are model-controlled: multi-byte runes must survive the cap.
	violations := []ArgViolation{{Path: strings.Repeat("\u00e9", 400), Reason: strings.Repeat("\u00e9", 400)}}
	summary := SummarizeArgViolations(violations)
	if !utf8.ValidString(summary) {
		t.Fatalf("invalid UTF-8 after truncation: %q", summary)
	}
	if len(summary) > 512 {
		t.Fatalf("summary over cap: %d", len(summary))
	}
}

func TestSanitizeViolationPath(t *testing.T) {
	got := sanitizeViolationPath("arg\nINJECTED\r\ttab:\u00e9")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("control characters survived: %q", got)
	}
	long := sanitizeViolationPath(strings.Repeat("\u00e9", 300))
	if !utf8.ValidString(long) {
		t.Errorf("rune split by path cap: %q", long)
	}
	if len(long) > 128 {
		t.Errorf("path over cap: %d", len(long))
	}
}

func TestValidateArgsEmptyPolicyFailsClosed(t *testing.T) {
	cfg := ArgGuardConfig{Enabled: true, MaxArgBytes: DefaultMaxArgBytes, URLPolicy: ""}
	_, rejected := ValidateArgs(map[string]any{"url": "http://127.0.0.1/"}, nil, cfg)
	if !rejected {
		t.Fatal("empty URL policy must normalize to deny_private")
	}
	unknown := cfg
	unknown.URLPolicy = "something-else"
	if _, rejected := ValidateArgs(map[string]any{"url": "http://127.0.0.1/"}, nil, unknown); !rejected {
		t.Fatal("unknown URL policy must normalize to deny_private")
	}
}

func TestValidateArgsNullOptionalProperty(t *testing.T) {
	schema := schemaFromJSON(t, `{"type":"object","properties":{"q":{"type":"string"}}}`)
	v, rejected := ValidateArgs(map[string]any{"q": nil}, schema, guard(true))
	if rejected || len(v) != 0 {
		t.Errorf("null optional value flagged: %v rejected=%v", v, rejected)
	}
	// Explicit null type must still be checked.
	nullSchema := schemaFromJSON(t, `{"type":"object","properties":{"q":{"type":"null"}}}`)
	if _, rejected := ValidateArgs(map[string]any{"q": nil}, nullSchema, guard(true)); rejected {
		t.Error("null against null type must pass")
	}
}

func TestValidateArgsURLWalkDepthCapFailsClosed(t *testing.T) {
	// Nest beyond the walk cap with a private URL at the bottom.
	var node any = "http://127.0.0.1/"
	for i := 0; i < maxWalkDepth+5; i++ {
		node = map[string]any{"n": node}
	}
	_, rejected := ValidateArgs(node.(map[string]any), nil, guard(true))
	if !rejected {
		t.Fatal("walk cap must fail closed")
	}
}

func TestGuardConfigFromServer(t *testing.T) {
	// nil knobs → disabled (guard inert).
	cfg := GuardConfigFromServer(nil, nil)
	if cfg.Enabled {
		t.Error("nil knobs must disable the guard")
	}
	// knobs with default byte cap materialize DefaultMaxArgBytes.
	if cfg := GuardConfigFromServer(&testKnobs{}, nil); cfg.MaxArgBytes != DefaultMaxArgBytes {
		t.Errorf("MaxArgBytes = %d, want default", cfg.MaxArgBytes)
	}
}

// testKnobs implements ArgGuardKnobs with unset values.
type testKnobs struct{}

func (testKnobs) EffectiveEnabled() bool     { return true }
func (testKnobs) EffectiveMaxArgBytes() int  { return 0 }
func (testKnobs) EffectiveURLPolicy() string { return "deny_private" }
