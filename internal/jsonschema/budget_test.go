package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// budgetTool is a realistic tool declaration (typical coding-agent shape).
func budgetTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "absolute path"},
					"lines": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
				},
				"required": []any{"path"},
			},
		},
	}
}

// nestedArray renders a schema nested exactly `depth` containers deep: the
// leaf string of nestedArray(0) is at depth 0, of nestedArray(100) at depth
// 100.
func nestedArray(depth int) any {
	var schema any = "leaf"
	for i := 0; i < depth; i++ {
		schema = []any{schema}
	}
	return schema
}

func TestCheckToolListBudget_AcceptsRealisticTools(t *testing.T) {
	tools := []any{
		budgetTool("read_file"),
		map[string]any{"name": "write_file", "input_schema": map[string]any{"type": "object"}},
	}
	if err := CheckToolListBudget(tools); err != nil {
		t.Fatalf("realistic tools must pass the budget: %v", err)
	}
}

func TestCheckToolListBudget_IgnoresUnexpectedShapes(t *testing.T) {
	tools := []any{"nope", 42, map[string]any{"type": "function"}, map[string]any{"function": "not-a-map"}}
	if err := CheckToolListBudget(tools); err != nil {
		t.Fatalf("expected the guard to pass unrecognized shapes through, got %v", err)
	}
	if err := CheckToolListBudget(nil); err != nil {
		t.Fatalf("an empty tool list must pass: %v", err)
	}
}

func TestCheckToolListBudget_NamesTheOffendingTool(t *testing.T) {
	deep := budgetTool("deep")
	deep["function"].(map[string]any)["parameters"] = nestedArray(MaxInputDepth + 1)

	err := CheckToolListBudget([]any{budgetTool("read_file"), deep})
	if err == nil {
		t.Fatal("expected the deep tool to be rejected")
	}
	if !strings.Contains(err.Error(), `"deep"`) {
		t.Fatalf("error must name the offending tool: %v", err)
	}
	if strings.Contains(err.Error(), `"read_file"`) {
		t.Fatalf("error must not name an innocent tool: %v", err)
	}
}

// wideTool returns a tool whose schema declares exactly perTool nodes: the
// array itself plus one element each.
func wideTool(nodes int) map[string]any {
	values := make([]any, nodes-1)
	for i := range values {
		values[i] = 1
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "wide", "parameters": values},
	}
}

func TestCheckToolListBudget_EnforcesTheAggregateNodeBudget(t *testing.T) {
	perTool := MaxInputNodes
	atLimit := MaxPayloadNodes / perTool
	tools := make([]any, atLimit+1)
	for i := range tools {
		tools[i] = wideTool(perTool)
	}
	// The exact aggregate limit passes: the tools array is not part of a
	// tool's schema.
	if err := CheckToolListBudget(tools[:atLimit]); err != nil {
		t.Fatalf("a tool list at exactly the aggregate limit must pass: %v", err)
	}
	budget := assertBudgetError(t, CheckToolListBudget(tools), "payload_nodes")
	if budget.Limit != MaxPayloadNodes {
		t.Fatalf("limit = %d, want the aggregate %d", budget.Limit, MaxPayloadNodes)
	}
	if budget.Observed != MaxPayloadNodes+perTool {
		t.Fatalf("observed = %d, want %d", budget.Observed, MaxPayloadNodes+perTool)
	}
}

func TestCheckSchemaBudget_RejectsDepth(t *testing.T) {
	budget := assertBudgetError(t, CheckSchemaBudget("deep", nestedArray(MaxInputDepth+50)), "depth")
	if budget.Observed != MaxInputDepth+1 {
		t.Fatalf("depth observed = %d, want the first overage %d", budget.Observed, MaxInputDepth+1)
	}
}

func TestCheckSchemaBudget_AcceptsExactlyMaxDepth(t *testing.T) {
	if err := CheckSchemaBudget("deep", nestedArray(MaxInputDepth)); err != nil {
		t.Fatalf("a schema at exactly the depth limit must pass: %v", err)
	}
	// A container at the limit with no children declares nothing deeper.
	var empty any = []any{}
	for i := 0; i < MaxInputDepth; i++ {
		empty = []any{empty}
	}
	if err := CheckSchemaBudget("empty", empty); err != nil {
		t.Fatalf("an empty container at the depth limit must pass: %v", err)
	}
}

func TestCheckSchemaBudget_RejectsNodes(t *testing.T) {
	// One array with a scalar per node, sized past the node cap. The rejection
	// must be O(1) in the container size (the walk accounts for the container
	// before pushing it), so this is a fail-fast assertion as much as a cap
	// assertion.
	wide := make([]any, MaxInputNodes+10)
	for i := range wide {
		wide[i] = 1
	}
	budget := assertBudgetError(t, CheckSchemaBudget("wide", wide), "nodes")
	if budget.Observed <= MaxInputNodes {
		t.Fatalf("nodes observed = %d, want more than the limit %d", budget.Observed, MaxInputNodes)
	}
}

func TestCheckSchemaBudget_AcceptsExactlyMaxNodes(t *testing.T) {
	// The array itself plus its elements is exactly the node limit.
	values := make([]any, MaxInputNodes-1)
	for i := range values {
		values[i] = "x"
	}
	if err := CheckSchemaBudget("exact", values); err != nil {
		t.Fatalf("a schema at exactly the node limit must pass: %v", err)
	}
}

func TestCheckSchemaBudget_RejectsBytesEarly(t *testing.T) {
	// One oversized string is rejected while it is visited — before the exact
	// encoded size is measured — so a schema carrying megabytes of text fails
	// fast.
	schema := map[string]any{"type": "string", "description": strings.Repeat("a", MaxInputBytes+1)}
	err := CheckSchemaBudget("huge", schema)
	budget := assertBudgetError(t, err, "bytes")
	if budget.Observed <= MaxInputBytes {
		t.Fatalf("bytes observed = %d, want more than the limit %d", budget.Observed, MaxInputBytes)
	}
}

func TestCheckSchemaBudget_AcceptsExactlyMaxBytes(t *testing.T) {
	// {"type":"string","description":"<padding>"} encodes to 2 + 13 + 2 + 15
	// + 2 + padding + 2 bytes with the two keys; size the padding so the
	// encoded form is exactly the limit.
	const overhead = len(`{"type":"string","description":""}`)
	schema := map[string]any{"type": "string", "description": strings.Repeat("a", MaxInputBytes-overhead)}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(encoded) != MaxInputBytes {
		t.Fatalf("fixture encodes to %d bytes, want exactly %d", len(encoded), MaxInputBytes)
	}
	if err := CheckSchemaBudget("exact", schema); err != nil {
		t.Fatalf("a schema at exactly the byte limit must pass: %v", err)
	}
}

func TestCheckResponseFormatBudget(t *testing.T) {
	tests := []struct {
		name    string
		format  map[string]any
		wantErr bool
	}{
		{name: "nil format"},
		{name: "json_object", format: map[string]any{"type": "json_object"}},
		{name: "json_schema without schema", format: map[string]any{"json_schema": map[string]any{"name": "out"}}},
		{name: "json_schema ok", format: map[string]any{"json_schema": map[string]any{
			"name": "out", "schema": map[string]any{"type": "object"},
		}}},
		{name: "json_schema too deep", wantErr: true, format: map[string]any{"json_schema": map[string]any{
			"name": "out", "schema": nestedArray(MaxInputDepth + 1),
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckResponseFormatBudget(tt.format)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected no rejection, got %v", err)
				}
				return
			}
			_ = assertBudgetError(t, err, "depth")
		})
	}
}

func TestBudgetError_MessageNamesDimensionAndTruncatesName(t *testing.T) {
	long := strings.Repeat("é", maxToolNameLen+10)
	err := &BudgetError{Tool: long, Dimension: "nodes", Limit: MaxInputNodes, Observed: MaxInputNodes + 1}
	message := err.Error()
	if !strings.Contains(message, "nodes") || !strings.Contains(message, fmt.Sprint(MaxInputNodes)) {
		t.Fatalf("message must name the dimension and limit: %s", message)
	}
	if strings.Contains(message, long) {
		t.Fatalf("message must truncate an oversized tool name: %s", message)
	}
	if !strings.Contains(message, "é") {
		t.Fatalf("truncation must be rune-safe: %s", message)
	}
}

func assertBudgetError(t *testing.T, err error, dimension string) *BudgetError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s budget violation", dimension)
	}
	var budget *BudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("expected a *BudgetError, got %T: %v", err, err)
	}
	if budget.Dimension != dimension {
		t.Fatalf("dimension = %q, want %q (%v)", budget.Dimension, dimension, err)
	}
	return budget
}

// FuzzCheckSchemaBudget asserts the budget walk never panics on arbitrary
// decoded JSON and that its own failures are always *BudgetError (the walk has
// no other failure mode), so a caller can classify a non-BudgetError as a
// server-side fault.
func FuzzCheckSchemaBudget(f *testing.F) {
	f.Add(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	f.Add(`[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]`)
	f.Add(`"scalar"`)
	f.Add(`{"a":`)
	f.Fuzz(func(t *testing.T, raw string) {
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			return
		}
		err := CheckSchemaBudget("fuzz", decoded)
		var budget *BudgetError
		if err != nil && !errors.As(err, &budget) {
			t.Fatalf("a budget walk must fail only with *BudgetError: %T %v", err, err)
		}
	})
}
