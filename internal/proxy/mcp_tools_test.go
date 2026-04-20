package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/internal/gateway"
	"nenya/internal/mcp"
)

func TestBufferStreamResponse_ContentOnly(t *testing.T) {
	sse := `data: {"id":"1","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"1","choices":[{"delta":{"content":" world"}}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`
	buf, err := bufferStreamResponse(context.Background(), strings.NewReader(sse))
	if err != nil {
		t.Fatalf("bufferStreamResponse failed: %v", err)
	}

	if !buf.hasContent {
		t.Fatal("expected hasContent=true")
	}
	if len(buf.toolCalls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(buf.toolCalls))
	}
	if buf.finishReason != "stop" {
		t.Fatalf("finishReason = %q, want stop", buf.finishReason)
	}
	if !strings.Contains(string(buf.rawBytes), "Hello") {
		t.Fatal("rawBytes missing content")
	}
	if !strings.Contains(string(buf.rawBytes), "[DONE]") {
		t.Fatal("rawBytes missing [DONE] marker")
	}
}

func TestBufferStreamResponse_ToolCalls(t *testing.T) {
	sse := `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"mempalace__mempalace_search","arguments":""}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"qu"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ery\":\"he"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"llo\"}"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	buf, err := bufferStreamResponse(context.Background(), strings.NewReader(sse))
	if err != nil {
		t.Fatalf("bufferStreamResponse failed: %v", err)
	}

	if buf.hasContent {
		t.Fatal("expected hasContent=false for tool call response")
	}
	if len(buf.toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(buf.toolCalls))
	}

	tc := buf.toolCalls[0]
	if tc.ID != "call_abc123" {
		t.Fatalf("tool call ID = %q, want call_abc123", tc.ID)
	}
	if tc.Name != "mempalace__mempalace_search" {
		t.Fatalf("tool call name = %q, want mempalace__mempalace_search", tc.Name)
	}
	if tc.Arguments["query"] != "hello" {
		t.Fatalf("tool call args = %v, want query=hello", tc.Arguments)
	}
	if buf.finishReason != "tool_calls" {
		t.Fatalf("finishReason = %q, want tool_calls", buf.finishReason)
	}
}

func TestBufferStreamResponse_Empty(t *testing.T) {
	buf, err := bufferStreamResponse(context.Background(), strings.NewReader(""))
	if err != nil {
		t.Fatalf("bufferStreamResponse failed: %v", err)
	}

	if buf.hasContent {
		t.Fatal("expected hasContent=false for empty stream")
	}
	if len(buf.rawBytes) != 0 {
		t.Fatalf("expected empty rawBytes, got %d bytes", len(buf.rawBytes))
	}
}

func TestBufferStreamResponse_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	_, err := bufferStreamResponse(ctx, strings.NewReader(sse))
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestPartitionMCPToolCalls(t *testing.T) {
	index := mcp.NewToolRegistry()
	index.Register("mempalace", []mcp.Tool{
		{Name: "search"},
		{Name: "status"},
	})

	calls := []mcpToolCall{
		{ID: "1", Name: "mempalace__search"},
		{ID: "2", Name: "file_edit"},
		{ID: "3", Name: "mempalace__status"},
		{ID: "4", Name: "bash"},
	}

	mcpCalls, nonMcpCalls := partitionMCPToolCalls(calls, index)

	if len(mcpCalls) != 2 {
		t.Fatalf("expected 2 MCP calls, got %d", len(mcpCalls))
	}
	if len(nonMcpCalls) != 2 {
		t.Fatalf("expected 2 non-MCP calls, got %d", len(nonMcpCalls))
	}

	for _, c := range mcpCalls {
		if c.Name != "mempalace__search" && c.Name != "mempalace__status" {
			t.Fatalf("unexpected MCP call: %s", c.Name)
		}
	}
	for _, c := range nonMcpCalls {
		if c.Name != "file_edit" && c.Name != "bash" {
			t.Fatalf("unexpected non-MCP call: %s", c.Name)
		}
	}
}

func TestPartitionMCPToolCalls_AllMCP(t *testing.T) {
	index := mcp.NewToolRegistry()
	index.Register("mempalace", []mcp.Tool{{Name: "search"}})

	calls := []mcpToolCall{
		{ID: "1", Name: "mempalace__search"},
	}

	mcpCalls, nonMcpCalls := partitionMCPToolCalls(calls, index)

	if len(mcpCalls) != 1 {
		t.Fatalf("expected 1 MCP call, got %d", len(mcpCalls))
	}
	if len(nonMcpCalls) != 0 {
		t.Fatalf("expected 0 non-MCP calls, got %d", len(nonMcpCalls))
	}
}

func TestPartitionMCPToolCalls_NoneMCP(t *testing.T) {
	index := mcp.NewToolRegistry()

	calls := []mcpToolCall{
		{ID: "1", Name: "file_edit"},
	}

	mcpCalls, nonMcpCalls := partitionMCPToolCalls(calls, index)

	if len(mcpCalls) != 0 {
		t.Fatalf("expected 0 MCP calls, got %d", len(mcpCalls))
	}
	if len(nonMcpCalls) != 1 {
		t.Fatalf("expected 1 non-MCP call, got %d", len(nonMcpCalls))
	}
}

func TestAppendMCPResults(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}

	assistantMsg := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{
			map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "mempalace__search",
					"arguments": `{"query":"test"}`,
				},
			},
		},
	}

	calls := []mcpToolCall{
		{ID: "call_1", Name: "mempalace__search", Arguments: map[string]any{"query": "test"}},
	}
	results := []*mcp.CallToolResult{
		{Content: []mcp.ContentBlock{{Type: "text", Text: "found 3 items"}}},
	}

	appendMCPResults(payload, calls, results, assistantMsg)

	messages, ok := payload["messages"].([]any)
	if !ok {
		t.Fatal("messages is not an array")
	}

	if len(messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, tool), got %d", len(messages))
	}

	assistMsg, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatal("second message is not a map")
	}
	if assistMsg["role"] != "assistant" {
		t.Fatalf("second message role = %v, want assistant", assistMsg["role"])
	}

	toolMsg, ok := messages[2].(map[string]any)
	if !ok {
		t.Fatal("third message is not a map")
	}
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool message role = %v, want tool", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", toolMsg["tool_call_id"])
	}
	if toolMsg["content"] != "found 3 items" {
		t.Fatalf("content = %v, want 'found 3 items'", toolMsg["content"])
	}
}

func TestAppendMCPResults_Error(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}

	assistantMsg := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "mcp__tool"}},
		},
	}

	calls := []mcpToolCall{{ID: "call_1", Name: "mcp__tool"}}
	results := []*mcp.CallToolResult{
		{Content: []mcp.ContentBlock{{Type: "text", Text: "server unavailable"}}, IsError: true},
	}

	appendMCPResults(payload, calls, results, assistantMsg)

	messages := payload["messages"].([]any)
	toolMsg := messages[2].(map[string]any)
	content := toolMsg["content"].(string)
	if content != "[MCP Error] server unavailable" {
		t.Fatalf("content = %q, want error prefix", content)
	}
}

func TestAppendMCPResults_NoMessages(t *testing.T) {
	payload := map[string]any{"messages": "not-array"}
	appendMCPResults(payload, []mcpToolCall{{ID: "1"}}, []*mcp.CallToolResult{{}}, nil)
}

func TestAppendMCPResults_NilResults(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "mcp__tool"}},
				},
			},
		},
	}

	appendMCPResults(payload, []mcpToolCall{{ID: "call_1"}}, nil, nil)
	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (unchanged), got %d", len(messages))
	}
}

func TestReplayBufferedResponse(t *testing.T) {
	w := httptest.NewRecorder()
	buf := &bufferedSSE{
		rawBytes: []byte("data: {\"choices\":[{}]}\n\ndata: [DONE]\n\n"),
	}

	replayBufferedResponse(w, buf, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: {\"choices\":[{}]}") {
		t.Fatalf("body missing SSE data: %q", body)
	}
}

func TestExecuteMCPCalls(t *testing.T) {
	mock := newTestMCPServer(t)
	defer mock.server.Close()

	client := mcp.NewClient(mcp.ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer client.Close()
	if _, err := client.RefreshTools(t.Context()); err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	toolIndex := mcp.NewToolRegistry()
	toolIndex.Register("mempalace", []mcp.Tool{
		{Name: "test_tool", Description: "A test tool"},
	})

	p := &Proxy{}
	p.StoreGateway(&gateway.NenyaGateway{
		MCPClients:   map[string]*mcp.Client{"mempalace": client},
		MCPToolIndex: toolIndex,
	})

	calls := []mcpToolCall{
		{ID: "1", Name: "mempalace__test_tool", Arguments: map[string]any{"query": "hello"}},
		{ID: "2", Name: "mempalace__test_tool", Arguments: map[string]any{"query": "world"}},
	}

	results := executeMCPCalls(t.Context(), calls, p.Gateway())

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0] == nil || results[0].Text() != "result for: hello" {
		t.Fatalf("result 0 = %q, want 'result for: hello'", results[0].Text())
	}
	if results[1] == nil || results[1].Text() != "result for: world" {
		t.Fatalf("result 1 = %q, want 'result for: world'", results[1].Text())
	}
}

func TestExecuteMCPCalls_UnknownTool(t *testing.T) {
	mock := newTestMCPServer(t)
	defer mock.server.Close()

	client := mcp.NewClient(mcp.ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer client.Close()
	if _, err := client.RefreshTools(t.Context()); err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	toolIndex := mcp.NewToolRegistry()
	p := &Proxy{}
	p.StoreGateway(&gateway.NenyaGateway{
		MCPClients:   map[string]*mcp.Client{"mempalace": client},
		MCPToolIndex: toolIndex,
	})

	calls := []mcpToolCall{
		{ID: "1", Name: "mempalace__unknown_tool"},
	}

	results := executeMCPCalls(t.Context(), calls, p.Gateway())

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatal("expected error for unknown tool")
	}
}

func TestExecuteMCPCalls_ServerUnavailable(t *testing.T) {
	toolIndex := mcp.NewToolRegistry()
	toolIndex.Register("mempalace", []mcp.Tool{{Name: "search"}})

	p := &Proxy{}
	p.StoreGateway(&gateway.NenyaGateway{
		MCPClients:   map[string]*mcp.Client{},
		MCPToolIndex: toolIndex,
	})

	calls := []mcpToolCall{
		{ID: "1", Name: "mempalace__search"},
	}

	results := executeMCPCalls(t.Context(), calls, p.Gateway())

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatal("expected error for unavailable server")
	}
	if !strings.Contains(results[0].Text(), "MCP server not available") {
		t.Fatalf("error text = %q, want server not available", results[0].Text())
	}
}

func TestExecuteMCPCalls_EmptyCalls(t *testing.T) {
	toolIndex := mcp.NewToolRegistry()
	p := &Proxy{}
	p.StoreGateway(&gateway.NenyaGateway{
		MCPClients:   map[string]*mcp.Client{},
		MCPToolIndex: toolIndex,
	})

	results := executeMCPCalls(t.Context(), nil, p.Gateway())
	if results != nil {
		t.Fatalf("expected nil results for empty calls, got %v", results)
	}
}

func TestBufferStreamResponse_MultipleToolCalls(t *testing.T) {
	sse := `data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"tool_a","arguments":""}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"tool_b","arguments":""}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"y\":2}"}}]}}]}

data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	buf, err := bufferStreamResponse(context.Background(), strings.NewReader(sse))
	if err != nil {
		t.Fatalf("bufferStreamResponse failed: %v", err)
	}

	if buf.hasContent {
		t.Fatal("expected hasContent=false")
	}
	if len(buf.toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(buf.toolCalls))
	}
	if buf.toolCalls[0].ID != "call_A" || buf.toolCalls[0].Name != "tool_a" {
		t.Errorf("tool call 0: got id=%q name=%q", buf.toolCalls[0].ID, buf.toolCalls[0].Name)
	}
	if buf.toolCalls[0].Arguments["x"] != float64(1) {
		t.Errorf("tool call 0 args: got %v", buf.toolCalls[0].Arguments)
	}
	if buf.toolCalls[1].ID != "call_B" || buf.toolCalls[1].Name != "tool_b" {
		t.Errorf("tool call 1: got id=%q name=%q", buf.toolCalls[1].ID, buf.toolCalls[1].Name)
	}
	if buf.toolCalls[1].Arguments["y"] != float64(2) {
		t.Errorf("tool call 1 args: got %v", buf.toolCalls[1].Arguments)
	}
}

func TestBufferStreamResponse_ContentAndToolCallsMixed(t *testing.T) {
	sse := `data: {"id":"1","choices":[{"delta":{"content":"Let me look that up."}}]}

data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_X","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]}}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	buf, err := bufferStreamResponse(context.Background(), strings.NewReader(sse))
	if err != nil {
		t.Fatalf("bufferStreamResponse failed: %v", err)
	}

	if !buf.hasContent {
		t.Fatal("expected hasContent=true")
	}
	if len(buf.toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(buf.toolCalls))
	}
	if buf.toolCalls[0].Name != "search" {
		t.Errorf("expected tool name 'search', got %q", buf.toolCalls[0].Name)
	}
	if buf.finishReason != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %q", buf.finishReason)
	}
}

func TestAppendMCPResults_MultipleCallsPreserveOrder(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "do things"},
		},
	}

	assistantMsg := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{
			map[string]any{"id": "call_A", "type": "function", "function": map[string]any{"name": "mcp__alpha", "arguments": `{}`}},
			map[string]any{"id": "call_B", "type": "function", "function": map[string]any{"name": "mcp__beta", "arguments": `{}`}},
			map[string]any{"id": "call_C", "type": "function", "function": map[string]any{"name": "mcp__gamma", "arguments": `{}`}},
		},
	}

	calls := []mcpToolCall{
		{ID: "call_A", Name: "mcp__alpha"},
		{ID: "call_B", Name: "mcp__beta"},
		{ID: "call_C", Name: "mcp__gamma"},
	}
	results := []*mcp.CallToolResult{
		{Content: []mcp.ContentBlock{{Type: "text", Text: "result A"}}},
		{Content: []mcp.ContentBlock{{Type: "text", Text: "result B"}}},
		{Content: []mcp.ContentBlock{{Type: "text", Text: "result C"}}},
	}

	appendMCPResults(payload, calls, results, assistantMsg)

	messages := payload["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages (user + assistant + 3 tool results), got %d", len(messages))
	}

	for i, expectedID := range []string{"call_A", "call_B", "call_C"} {
		toolMsg := messages[2+i].(map[string]any)
		if toolMsg["tool_call_id"] != expectedID {
			t.Errorf("message %d: expected tool_call_id=%q, got %q", 2+i, expectedID, toolMsg["tool_call_id"])
		}
	}
	if messages[2].(map[string]any)["content"] != "result A" {
		t.Errorf("expected result A, got %v", messages[2].(map[string]any)["content"])
	}
	if messages[3].(map[string]any)["content"] != "result B" {
		t.Errorf("expected result B, got %v", messages[3].(map[string]any)["content"])
	}
	if messages[4].(map[string]any)["content"] != "result C" {
		t.Errorf("expected result C, got %v", messages[4].(map[string]any)["content"])
	}
}

func TestAppendMCPResults_LargeContentNotTruncated(t *testing.T) {
	largeContent := strings.Repeat("x", 10000)

	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
		},
	}

	assistantMsg := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "mcp__big"}},
		},
	}

	appendMCPResults(payload, []mcpToolCall{{ID: "call_1", Name: "mcp__big"}},
		[]*mcp.CallToolResult{{Content: []mcp.ContentBlock{{Type: "text", Text: largeContent}}}},
		assistantMsg)

	messages := payload["messages"].([]any)
	toolMsg := messages[2].(map[string]any)
	content := toolMsg["content"].(string)
	if len(content) != 10000 {
		t.Fatalf("expected 10000 bytes of content, got %d", len(content))
	}
}
