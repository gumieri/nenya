package stream

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nenya/internal/util"
)

// normalizeChunks feeds SSE data lines through a passthrough reader and
// returns the forwarded JSON chunks.
func normalizeChunks(t *testing.T, lines []string) []map[string]interface{} {
	t.Helper()
	input := strings.Join(lines, "\n\n") + "\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	out, err := io.ReadAll(reader)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected read error: %v", err)
	}
	var chunks []map[string]interface{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var c map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("forwarded non-JSON data line: %q (%v)", line, err)
		}
		chunks = append(chunks, c)
	}
	return chunks
}

func toolCallsFrom(chunks []map[string]interface{}, choiceIdx int) []interface{} {
	var out []interface{}
	for _, c := range chunks {
		choices, _ := c["choices"].([]interface{})
		if len(choices) <= choiceIdx {
			continue
		}
		choice, _ := choices[choiceIdx].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if tcs, ok := delta["tool_calls"].([]interface{}); ok {
			out = append(out, tcs...)
		}
	}
	return out
}

// TestToolCallIdentity_OmittedIndexFallsBackToArrayPosition pins the
// be1763e5 rule: two calls in one delta without index fields get distinct
// deterministic IDs from their array positions — never 0-for-all.
func TestToolCallIdentity_OmittedIndexFallsBackToArrayPosition(t *testing.T) {
	chunks := normalizeChunks(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[` +
			`{"id":"","function":{"name":"read_file","arguments":"{}"}},` +
			`{"id":"","function":{"name":"write_file","arguments":"{}"}}]}}]}`,
	})
	tcs := toolCallsFrom(chunks, 0)
	if len(tcs) != 2 {
		t.Fatalf("expected both tool calls forwarded, got %d", len(tcs))
	}
	id0, _ := tcs[0].(map[string]interface{})["id"].(string)
	id1, _ := tcs[1].(map[string]interface{})["id"].(string)
	if id0 != util.SyntheticToolCallID(0) {
		t.Errorf("first call should take call_0, got %q", id0)
	}
	if id1 != util.SyntheticToolCallID(1) {
		t.Errorf("second call must fall back to array position (call_1), got %q", id1)
	}
}

// TestToolCallIdentity_DuplicateUpstreamIDDeconflicted pins db143aeb: an
// upstream ID already claimed by another index is deterministically
// re-keyed — first claimant keeps it, the later one gets the synthetic ID.
func TestToolCallIdentity_DuplicateUpstreamIDDeconflicted(t *testing.T) {
	chunks := normalizeChunks(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[` +
			`{"index":0,"id":"upstream_x","function":{"name":"a","arguments":"{}"}},` +
			`{"index":1,"id":"upstream_x","function":{"name":"b","arguments":"{}"}}]}}]}`,
	})
	tcs := toolCallsFrom(chunks, 0)
	if len(tcs) != 2 {
		t.Fatalf("expected both tool calls forwarded, got %d", len(tcs))
	}
	id0, _ := tcs[0].(map[string]interface{})["id"].(string)
	id1, _ := tcs[1].(map[string]interface{})["id"].(string)
	if id0 != "upstream_x" {
		t.Errorf("first claimant must keep the upstream ID, got %q", id0)
	}
	if id1 != util.SyntheticToolCallID(1) {
		t.Errorf("duplicate must be re-keyed deterministically, got %q", id1)
	}
}

// TestToolCallIdentity_InvalidIDsReplaced covers empty, non-string, and
// oversized upstream IDs — all replaced with the deterministic synthetic ID.
func TestToolCallIdentity_InvalidIDsReplaced(t *testing.T) {
	oversized := strings.Repeat("x", 200)
	chunks := normalizeChunks(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[` +
			`{"index":0,"id":"","function":{"name":"a","arguments":"{}"}},` +
			`{"index":1,"id":` + mustJSON(t, oversized) + `,"function":{"name":"b","arguments":"{}"}}]}}]}`,
	})
	tcs := toolCallsFrom(chunks, 0)
	if len(tcs) != 2 {
		t.Fatalf("expected both tool calls forwarded, got %d", len(tcs))
	}
	for i, want := range []string{
		util.SyntheticToolCallID(0),
		util.SyntheticToolCallID(1),
	} {
		got, _ := tcs[i].(map[string]interface{})["id"].(string)
		if got != want {
			t.Errorf("call %d: expected %q, got %q", i, want, got)
		}
	}
}

// TestToolCallIdentity_ValidIDsPreserved checks round-trip stability: valid
// upstream IDs pass through untouched — ThoughtSigCache replay keys on them.
func TestToolCallIdentity_ValidIDsPreserved(t *testing.T) {
	chunks := normalizeChunks(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc123","function":{"name":"a","arguments":"{}"}}]}}]}`,
	})
	tcs := toolCallsFrom(chunks, 0)
	if len(tcs) != 1 {
		t.Fatalf("expected the call forwarded, got %d", len(tcs))
	}
	if id, _ := tcs[0].(map[string]interface{})["id"].(string); id != "call_abc123" {
		t.Errorf("valid upstream ID must be preserved, got %q", id)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
