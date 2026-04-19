package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"nenya/internal/mcp"
)

type testMCPServer struct {
	t       testing.TB
	mu      sync.Mutex
	tools   []mcpToolDef
	handles map[string]func(map[string]any) *mcp.CallToolResult
	server  *httptest.Server
}

type mcpToolDef struct {
	Name        string
	Description string
	InputSchema mcp.InputSchema
}

func newTestMCPServer(t testing.TB) *testMCPServer {
	t.Helper()
	ms := &testMCPServer{
		t:       t,
		handles: make(map[string]func(map[string]any) *mcp.CallToolResult),
		tools: []mcpToolDef{
			{
				Name: "test_tool", Description: "A test tool",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"query": map[string]any{"type": "string"},
					},
					Required: []string{"query"},
				},
			},
		},
	}

	ms.handles["test_tool"] = func(args map[string]any) *mcp.CallToolResult {
		query, _ := args["query"].(string)
		return &mcp.CallToolResult{
			Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("result for: %s", query)}},
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", ms.handleSSE)
	mux.HandleFunc("/message", ms.handleMessage)
	ms.server = httptest.NewServer(mux)
	t.Cleanup(func() { ms.server.Close() })

	return ms
}

func (ms *testMCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	endpointURL := ms.server.URL + "/message"
	endpointJSON, _ := json.Marshal(map[string]string{"endpoint": endpointURL})
	fmt.Fprintf(w, "data: %s\n\n", endpointJSON)
	flusher.Flush()

	<-r.Context().Done()
}

func (ms *testMCPServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req mcp.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		result := mcp.InitializeResult{
			ProtocolVersion: "2025-03-26",
			Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
			ServerInfo:      mcp.ImplementationInfo{Name: "mock-mcp", Version: "0.1.0"},
		}
		ms.writeRPCResponse(w, req.ID, result)
	case "ping":
		ms.writeRPCResponse(w, req.ID, map[string]string{"status": "ok"})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		ms.mu.Lock()
		tools := ms.tools
		ms.mu.Unlock()
		var mcpTools []mcp.Tool
		for _, td := range tools {
			mcpTools = append(mcpTools, mcp.Tool{
				Name:        td.Name,
				Description: td.Description,
				InputSchema: td.InputSchema,
			})
		}
		ms.writeRPCResponse(w, req.ID, mcp.ListToolsResult{Tools: mcpTools})
	case "tools/call":
		var params mcp.CallToolParams
		paramsBytes, _ := json.Marshal(req.Params)
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			ms.writeRPCError(w, req.ID, mcp.ErrCodeInvalidParams, "invalid params")
			return
		}
		ms.mu.Lock()
		handler, ok := ms.handles[params.Name]
		ms.mu.Unlock()
		if !ok {
			ms.writeRPCError(w, req.ID, mcp.ErrCodeMethodNotFound, "unknown tool: "+params.Name)
			return
		}
		result := handler(params.Arguments)
		ms.writeRPCResponse(w, req.ID, result)
	default:
		ms.writeRPCError(w, req.ID, mcp.ErrCodeMethodNotFound, "unknown method")
	}
}

func (ms *testMCPServer) writeRPCResponse(w http.ResponseWriter, id any, result any) {
	resp := mcp.Response{JSONRPC: mcp.JSONRPCVersion2, ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	if _, err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("failed to encode response: %v", err)
	}
}

func (ms *testMCPServer) writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := mcp.Response{
		JSONRPC: mcp.JSONRPCVersion2, ID: id,
		Error: &mcp.Error{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
