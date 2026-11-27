package pipeline

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// NENYA-348 pins: the tool-call arguments surface runs the JSON-aware
// redactor — a secret hidden behind \" or \uXXXX quoting inside the
// encoded arguments document is caught (decoded span replaced, structure
// preserved), while flat-only surfaces keep their flat semantics.

var argsTestPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token)[\s:=]+['"][^'"]{6,}['"]`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

func argsRedactInterceptor() *RedactInterceptor {
	return NewRedactInterceptor(true, argsTestPatterns, "REDACTED", nil)
}

// argsMessage builds a one-tool-call assistant message.
func argsMessage(arguments string) []map[string]any {
	return []map[string]any{{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "run_cmd",
				"arguments": arguments,
			},
		}},
	}}
}

func processArgs(t *testing.T, interceptor *RedactInterceptor, arguments string) map[string]any {
	t.Helper()
	msgs := argsMessage(arguments)
	req := &InterceptRequest{
		Payload:  map[string]any{"model": "demo"},
		Messages: msgs,
	}
	if _, err := interceptor.Process(t.Context(), req); err != nil {
		t.Fatalf("process: %v", err)
	}
	call := req.Messages[0]["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	out, _ := fn["arguments"].(string)
	return map[string]any{"args": out, "msg": req.Messages[0]}
}

func TestRedactInterceptorJSONEscapedArgsRedacted(t *testing.T) {
	out := processArgs(t, argsRedactInterceptor(), `{"cmd":"mysql -u root --password=\"hunter2secret\" -e 'select 1'","dir":"/tmp"}`)
	if strings.Contains(out["args"].(string), "hunter2secret") {
		t.Fatalf("escaped secret survived: %s", out["args"])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out["args"].(string)), &decoded); err != nil {
		t.Fatalf("rewritten arguments are not valid JSON: %v (%s)", err, out["args"])
	}
	if decoded["dir"] != "/tmp" {
		t.Fatalf("untouched sibling value lost: %v", decoded["dir"])
	}
	if !strings.Contains(out["args"].(string), "REDACTED") {
		t.Fatalf("label missing: %s", out["args"])
	}
}

func TestRedactInterceptorUnicodeEscapedArgsRedacted(t *testing.T) {
	out := processArgs(t, argsRedactInterceptor(), `{"q":"set password=\u0022hunter2secret\u0022 in config"}`)
	if strings.Contains(out["args"].(string), "hunter2secret") {
		t.Fatalf("unicode-escaped secret survived: %s", out["args"])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out["args"].(string)), &decoded); err != nil {
		t.Fatalf("rewritten arguments are not valid JSON: %v (%s)", err, out["args"])
	}
}

func TestRedactInterceptorFlatArgsStillCaught(t *testing.T) {
	out := processArgs(t, argsRedactInterceptor(), `{"cmd":"export KEY=AKIAIOSFODNN7EXAMPLE"}`)
	if strings.Contains(out["args"].(string), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("flat-detectable secret survived: %s", out["args"])
	}
}

func TestRedactInterceptorContentKeepsFlatSemantics(t *testing.T) {
	// The content surface is NOT JSON-decoded: an escaped-looking string
	// there keeps its flat behavior (the credentials pattern requires a
	// real quote; the backslash-quote form does not match).
	msgs := []map[string]any{{
		"role":    "user",
		"content": `the config line is password=\"hunter2secret\" verbatim`,
	}}
	req := &InterceptRequest{Payload: map[string]any{"model": "demo"}, Messages: msgs}
	if _, err := argsRedactInterceptor().Process(t.Context(), req); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !strings.Contains(req.Messages[0]["content"].(string), "hunter2secret") {
		t.Fatal("flat content surface was decoded and rewritten")
	}
}

func TestRedactInterceptorInvalidJSONArgsNeutralized(t *testing.T) {
	// Truncated JSON with a flat-detectable secret: the flat pass
	// redacts, the document stays invalid, and the walker neutralizes
	// wholesale (the original text never survives).
	out := processArgs(t, argsRedactInterceptor(), `{"cmd":"export KEY=AKIAIOSFODNN7EXAMPLE"`)
	if out["args"].(string) != `{"nenya":"content_neutralized"}` {
		t.Fatalf("invalid-JSON arguments not neutralized: %q", out["args"])
	}
}

func TestRedactInterceptorCleanArgsUntouched(t *testing.T) {
	args := `{"path":"/etc/nginx/nginx.conf","lines":[1,2]}`
	out := processArgs(t, argsRedactInterceptor(), args)
	if out["args"].(string) != args {
		t.Fatalf("clean arguments rewritten: %q", out["args"])
	}
}

// TestRedactInterceptorStructuralCrossingMatchClamped pins the clamp
// semantics for a structure-crossing match (the b4a69a88 fuzz class): an
// escaped quote aligning with the literal's structural terminator — the
// match's in-literal portion is redacted (the whole secret here) and the
// structural bytes survive, keeping the document valid.
func TestRedactInterceptorStructuralCrossingMatchClamped(t *testing.T) {
	out := processArgs(t, argsRedactInterceptor(), `{"0":"pAsswd \"0000000000"}`)
	args := out["args"].(string)
	if strings.Contains(args, "0000000000") {
		t.Fatalf("crossing match not redacted: %q", args)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("clamped rewrite broke JSON: %v (%q)", err, args)
	}
	if !strings.HasPrefix(args, `{"0":"`) || !strings.HasSuffix(args, `"}`) {
		t.Fatalf("structural bytes damaged: %q", args)
	}
}

// TestRedactInterceptorInteriorCrossingRunsRedacted pins the two-literal
// interior case (operator pattern spanning structure): each literal
// fragment the match touched is redacted independently and the JSON
// structure survives — no whole-args neutralization and no silent
// element merge.
func TestRedactInterceptorInteriorCrossingRunsRedacted(t *testing.T) {
	crossing := []*regexp.Regexp{regexp.MustCompile(`secret","mo`)}
	interceptor := NewRedactInterceptor(true, crossing, "REDACTED", nil)
	out := processArgs(t, interceptor, `{"path":"a secret","mode":"w"}`)
	args := out["args"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("interior-crossing rewrite broke JSON: %v (%q)", err, args)
	}
	// Runs: `secret` in the value literal, `mo` in the key literal —
	// each redacted independently, the structure untouched ("mode"
	// loses its matched prefix: REDACTED + "de").
	if decoded["path"] != "a REDACTED" || decoded["REDACTEDde"] != "w" {
		t.Fatalf("per-run redaction wrong: %q (decoded %+v)", args, decoded)
	}
	// The array shape: both fragments redacted, both elements survive.
	array := []*regexp.Regexp{regexp.MustCompile(`secret","x`)}
	arrayInterceptor := NewRedactInterceptor(true, array, "REDACTED", nil)
	out2 := processArgs(t, arrayInterceptor, `["a secret","x"]`)
	var decodedArr []any
	if err := json.Unmarshal([]byte(out2["args"].(string)), &decodedArr); err != nil {
		t.Fatalf("array rewrite broke JSON: %v (%q)", err, out2["args"])
	}
	if len(decodedArr) != 2 || decodedArr[0] != "a REDACTED" || decodedArr[1] != "REDACTED" {
		t.Fatalf("array runs redacted wrong: %q (decoded %+v)", out2["args"], decodedArr)
	}
}
