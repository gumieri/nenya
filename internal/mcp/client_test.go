package mcp

import (
	"encoding/json"
	"testing"
	"time"
)

func TestClient_Initialize(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	err := client.Initialize(t.Context())
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if !client.Ready() {
		t.Fatal("expected client to be ready after Initialize")
	}

	info := client.ServerInfo()
	if info.Name != "mock-mcp" {
		t.Fatalf("ServerInfo.Name = %q, want %q", info.Name, "mock-mcp")
	}

	_ = client.Close()
}

func TestClient_RefreshTools(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.RefreshTools(t.Context())
	if err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}
	if !toolNames["test_tool"] {
		t.Fatal("expected test_tool in tools list")
	}
	if !toolNames["status_tool"] {
		t.Fatal("expected status_tool in tools list")
	}
}

func TestClient_ListTools_Cached(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err := client.RefreshTools(t.Context())
	if err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	cached := client.ListTools()
	if len(cached) != 2 {
		t.Fatalf("expected 2 cached tools, got %d", len(cached))
	}
}

func TestClient_GetTool(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, _ = client.RefreshTools(t.Context())

	tool, ok := client.GetTool("test_tool")
	if !ok {
		t.Fatal("expected test_tool to exist")
	}
	if tool.Description != "A test tool" {
		t.Fatalf("description = %q, want %q", tool.Description, "A test tool")
	}

	_, ok = client.GetTool("nonexistent")
	if ok {
		t.Fatal("expected nonexistent tool to not exist")
	}
}

func TestClient_CallTool(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, _ = client.RefreshTools(t.Context())

	result, err := client.CallTool(t.Context(), "test_tool", map[string]any{"query": "test query"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	if result.IsError {
		t.Fatal("expected IsError to be false")
	}

	text := result.Text()
	if text != "result for: test query" {
		t.Fatalf("Text() = %q, want %q", text, "result for: test query")
	}
}

func TestClient_CallTool_UnknownTool(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, _ = client.RefreshTools(t.Context())

	_, err := client.CallTool(t.Context(), "nonexistent_tool", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestClient_CallTool_NotInitialized(t *testing.T) {
	client := NewClient(ClientConfig{
		URL:    "http://127.0.0.1:1/sse",
		Logger: newTestLogger(),
	})

	_, err := client.CallTool(t.Context(), "test_tool", nil)
	if err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestClient_CallTool_ServerError(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, _ = client.RefreshTools(t.Context())

	_, err := client.CallTool(t.Context(), "nonexistent_tool", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestClient_Ping(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Ping(t.Context()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestClient_Close(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	_ = client.Close()

	if client.Ready() {
		t.Fatal("expected client to not be ready after Close")
	}

	if err := client.Ping(t.Context()); err == nil {
		t.Fatal("expected error from Ping after Close")
	}
}

func TestClient_Close_Idempotent(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	_ = client.Close()
	_ = client.Close()

	if client.Ready() {
		t.Fatal("expected client to not be ready after double Close")
	}
}

func TestClient_ServerName(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	if client.ServerName() != "mock-mcp" {
		t.Fatalf("ServerName() = %q, want %q", client.ServerName(), "mock-mcp")
	}
}

func TestClient_CallToolResult_Text_MultipleBlocks(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{
			{Type: "text", Text: "line one"},
			{Type: "text", Text: "line two"},
		},
	}

	text := result.Text()
	if text != "line one\nline two" {
		t.Fatalf("Text() = %q, want %q", text, "line one\nline two")
	}
}

func TestClient_CallToolResult_Text_Empty(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{},
	}

	text := result.Text()
	if text != "" {
		t.Fatalf("Text() = %q, want empty", text)
	}
}

func TestClient_CallToolResult_Text_NonTextIgnored(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{
			{Type: "image", Text: ""},
			{Type: "text", Text: "visible"},
			{Type: "image", Text: ""},
		},
	}

	text := result.Text()
	if text != "visible" {
		t.Fatalf("Text() = %q, want %q", text, "visible")
	}
}

func TestClient_NewClient_Defaults(t *testing.T) {
	client := NewClient(ClientConfig{})

	if client.name != "nenya" {
		t.Fatalf("default name = %q, want %q", client.name, "nenya")
	}

	if client.Ready() {
		t.Fatal("new client should not be ready")
	}
}

func TestClient_Initialize_InvalidURL(t *testing.T) {
	client := NewClient(ClientConfig{
		URL:    "://invalid",
		Logger: newTestLogger(),
	})

	err := client.Initialize(t.Context())
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestClient_Initialize_ServerDown(t *testing.T) {
	client := NewClient(ClientConfig{
		URL:            "http://127.0.0.1:1/sse",
		ConnectTimeout: 1 * time.Second,
		Logger:         newTestLogger(),
	})

	err := client.Initialize(t.Context())
	if err == nil {
		t.Fatal("expected error for server down")
	}
}

func TestCallToolParams_Marshal(t *testing.T) {
	params := CallToolParams{
		Name: "test_tool",
		Arguments: map[string]any{
			"query": "hello",
			"limit": 5,
		},
	}

	bytes, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var unmarshaled map[string]any
	if err := json.Unmarshal(bytes, &unmarshaled); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if unmarshaled["name"] != "test_tool" {
		t.Fatalf("name = %v, want %v", unmarshaled["name"], "test_tool")
	}
}

func TestTool_InputSchema(t *testing.T) {
	schema := InputSchema{
		Type: "object",
		Properties: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "search query",
			},
		},
		Required: []string{"query"},
	}

	bytes, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(bytes, &result); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if result["type"] != "object" {
		t.Fatalf("type = %v, want object", result["type"])
	}

	required, ok := result["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %v, want [query]", result["required"])
	}
}
