package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nenya/internal/testutil"
)

func newTestLogger() *slog.Logger {
	return testutil.NewTestLogger()
}

type mockMCPServer struct {
	t       *testing.T
	mu      sync.Mutex
	tools   []Tool
	handles map[string]func(map[string]any) *CallToolResult

	postMux *http.ServeMux
	server  *httptest.Server
}

func newMockMCPServer(t *testing.T) *mockMCPServer {
	t.Helper()
	ms := &mockMCPServer{
		t:       t,
		handles: make(map[string]func(map[string]any) *CallToolResult),
		tools: []Tool{
			{
				Name:        "test_tool",
				Description: "A test tool",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]any{
						"query": map[string]any{"type": "string", "description": "search query"},
						"limit": map[string]any{"type": "integer", "description": "max results"},
					},
					Required: []string{"query"},
				},
			},
			{
				Name:        "status_tool",
				Description: "Returns status",
				InputSchema: InputSchema{Type: "object", Properties: map[string]any{}},
			},
		},
	}

	ms.handles["test_tool"] = func(args map[string]any) *CallToolResult {
		query, _ := args["query"].(string)
		return &CallToolResult{
			Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf("result for: %s", query)}},
		}
	}
	ms.handles["status_tool"] = func(args map[string]any) *CallToolResult {
		return &CallToolResult{
			Content: []ContentBlock{{Type: "text", Text: "all systems operational"}},
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", ms.handleSSE)
	mux.HandleFunc("/message", ms.handleMessage)
	ms.postMux = mux

	ms.server = httptest.NewServer(ms.postMux)
	t.Cleanup(func() { ms.server.Close() })

	return ms
}

func (ms *mockMCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		ms.t.Fatal("mock server: response writer not a flusher")
	}

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

func (ms *mockMCPServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		result := InitializeResult{
			ProtocolVersion: "2025-03-26",
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
			ServerInfo:      ImplementationInfo{Name: "mock-mcp", Version: "0.1.0"},
		}
		ms.writeRPCResponse(w, req.ID, result)

	case "ping":
		ms.writeRPCResponse(w, req.ID, map[string]string{"status": "ok"})

	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "{}")

	case "tools/list":
		ms.mu.Lock()
		tools := ms.tools
		ms.mu.Unlock()
		ms.writeRPCResponse(w, req.ID, ListToolsResult{Tools: tools})

	case "tools/call":
		var params CallToolParams
		paramsBytes, _ := json.Marshal(req.Params)
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			ms.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params")
			return
		}

		ms.mu.Lock()
		handler, ok := ms.handles[params.Name]
		ms.mu.Unlock()

		if !ok {
			ms.writeRPCError(w, req.ID, ErrCodeMethodNotFound, "unknown tool: "+params.Name)
			return
		}

		result := handler(params.Arguments)
		ms.writeRPCResponse(w, req.ID, result)

	default:
		ms.writeRPCError(w, req.ID, ErrCodeMethodNotFound, "unknown method: "+req.Method)
	}
}

func (ms *mockMCPServer) writeRPCResponse(w http.ResponseWriter, id any, result any) {
	resp := Response{
		JSONRPC: JSONRPCVersion2,
		ID:      id,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		ms.t.Fatalf("failed to encode response: %v", err)
	}
}

func (ms *mockMCPServer) writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := Response{
		JSONRPC: JSONRPCVersion2,
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		ms.t.Fatalf("failed to encode response: %v", err)
	}
}

func TestHTTPTransport_Connect(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	err := transport.Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	if !transport.Ready() {
		t.Fatal("expected transport to be ready after Connect")
	}

	expected := mock.server.URL + "/message"
	if got := transport.SessionEndpoint(); got != expected {
		t.Fatalf("SessionEndpoint = %q, want %q", got, expected)
	}

	transport.Close()
}

func TestHTTPTransport_Connect_InvalidURL(t *testing.T) {
	transport := NewHTTPTransport(TransportConfig{
		URL:    "://invalid-url",
		Logger: newTestLogger(),
	})

	err := transport.Connect(t.Context())
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestHTTPTransport_Connect_ServerUnavailable(t *testing.T) {
	transport := NewHTTPTransport(TransportConfig{
		URL:            "http://127.0.0.1:1/sse",
		ConnectTimeout: 1 * time.Second,
		Logger:         newTestLogger(),
	})

	err := transport.Connect(t.Context())
	if err == nil {
		t.Fatal("expected error for unavailable server")
	}
}

func TestHTTPTransport_Close(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	transport.Close()

	if transport.Ready() {
		t.Fatal("expected transport to not be ready after Close")
	}
}

func TestHTTPTransport_Close_Idempotent(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	transport.Close()
	transport.Close()

	if transport.Ready() {
		t.Fatal("expected transport to not be ready after double Close")
	}
}

func TestHTTPTransport_SendRequest_Initialize(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	params := InitializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    ClientCapabilities{},
		ClientInfo:      ImplementationInfo{Name: "nenya-test", Version: "0.0.1"},
	}

	resp, err := transport.SendRequest(t.Context(), "initialize", params)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	if serverInfo, ok := resultMap["serverInfo"].(map[string]any); ok {
		if name, _ := serverInfo["name"].(string); name != "mock-mcp" {
			t.Fatalf("serverInfo.name = %q, want %q", name, "mock-mcp")
		}
	}
}

func TestHTTPTransport_SendRequest_ToolsList(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	resp, err := transport.SendRequest(t.Context(), "tools/list", nil)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	toolsRaw, ok := resultMap["tools"].([]any)
	if !ok {
		t.Fatal("expected tools array in result")
	}
	if len(toolsRaw) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(toolsRaw))
	}
}

func TestHTTPTransport_SendRequest_ToolCall(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	params := CallToolParams{
		Name:      "test_tool",
		Arguments: map[string]any{"query": "hello world"},
	}

	resp, err := transport.SendRequest(t.Context(), "tools/call", params)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	content, ok := resultMap["content"].([]any)
	if !ok {
		t.Fatal("expected content array in result")
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatal("expected content block to be a map")
	}
	if text, _ := block["text"].(string); text != "result for: hello world" {
		t.Fatalf("text = %q, want %q", text, "result for: hello world")
	}
}

func TestHTTPTransport_SendRequest_ErrorResponse(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	params := CallToolParams{
		Name: "nonexistent_tool",
	}

	resp, err := transport.SendRequest(t.Context(), "tools/call", params)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp != nil && resp.Error == nil {
		t.Fatal("expected error object in response")
	}
	if resp != nil && resp.Error.Code != ErrCodeMethodNotFound {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, ErrCodeMethodNotFound)
	}
}

func TestHTTPTransport_SendRequest_NotConnected(t *testing.T) {
	transport := NewHTTPTransport(TransportConfig{
		URL:    "http://127.0.0.1:1/sse",
		Logger: newTestLogger(),
	})

	_, err := transport.SendRequest(t.Context(), "ping", nil)
	if err != ErrTransportNotReady {
		t.Fatalf("expected ErrTransportNotReady, got %v", err)
	}
}

func TestHTTPTransport_SendRequest_AfterClose(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	transport.Close()

	_, err := transport.SendRequest(t.Context(), "ping", nil)
	if err != ErrTransportClosed {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

func TestHTTPTransport_SendNotification(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	err := transport.SendNotification("notifications/initialized", nil)
	if err != nil {
		t.Fatalf("SendNotification failed: %v", err)
	}
}

func TestHTTPTransport_SendNotification_NotConnected(t *testing.T) {
	transport := NewHTTPTransport(TransportConfig{
		URL:    "http://127.0.0.1:1/sse",
		Logger: newTestLogger(),
	})

	err := transport.SendNotification("notifications/initialized", nil)
	if err != ErrTransportClosed {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

func TestHTTPTransport_Defaults(t *testing.T) {
	cfg := TransportConfig{URL: "http://localhost/sse", Logger: newTestLogger()}
	cfg.setDefaults()

	if cfg.ConnectTimeout != 10*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 10s", cfg.ConnectTimeout)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Fatalf("RequestTimeout = %v, want 30s", cfg.RequestTimeout)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %v, want 60s", cfg.IdleTimeout)
	}
	if cfg.ReconnectBackoff != 30*time.Second {
		t.Fatalf("ReconnectBackoff = %v, want 30s", cfg.ReconnectBackoff)
	}
}

func TestHTTPTransport_HeadersPassed(t *testing.T) {
	mock := newMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
		Headers: map[string]string{
			"X-Custom-Auth": "[REDACTED]",
			"X-Request-Id":  "req-123",
		},
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	resp, err := transport.SendRequest(t.Context(), "ping", nil)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}
	if status, _ := resultMap["status"].(string); status != "ok" {
		t.Fatalf("status = %q, want %q", status, "ok")
	}
}

type proxyMockMCPServer struct {
	t      *testing.T
	sseCh  chan sseOutgoing
	server *httptest.Server
}

type sseOutgoing struct {
	data string
}

func newProxyMockMCPServer(t *testing.T) *proxyMockMCPServer {
	t.Helper()
	pm := &proxyMockMCPServer{
		t:     t,
		sseCh: make(chan sseOutgoing, 64),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", pm.handleSSE)
	mux.HandleFunc("/message", pm.handleMessage)
	pm.server = httptest.NewServer(mux)
	t.Cleanup(func() { pm.server.Close() })

	return pm
}

func (pm *proxyMockMCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		pm.t.Fatal("proxy mock: response writer not a flusher")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	endpointURL := pm.server.URL + "/message"
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURL)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-pm.sseCh:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg.data)
			flusher.Flush()
		}
	}
}

func (pm *proxyMockMCPServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		result := InitializeResult{
			ProtocolVersion: "2025-03-26",
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
			ServerInfo:      ImplementationInfo{Name: "proxy-mock", Version: "0.1.0"},
		}
		resp := Response{JSONRPC: JSONRPCVersion2, ID: req.ID, Result: result}
		respBytes, _ := json.Marshal(resp)
		pm.sseCh <- sseOutgoing{data: string(respBytes)}

	case "tools/list":
		resp := Response{
			JSONRPC: JSONRPCVersion2,
			ID:      req.ID,
			Result: ListToolsResult{Tools: []Tool{
				{
					Name:        "proxy_tool",
					Description: "A proxy tool",
					InputSchema: InputSchema{Type: "object", Properties: map[string]any{
						"query": map[string]any{"type": "string"},
					}},
				},
			}},
		}
		respBytes, _ := json.Marshal(resp)
		pm.sseCh <- sseOutgoing{data: string(respBytes)}

	case "tools/call":
		var params CallToolParams
		paramsBytes, _ := json.Marshal(req.Params)
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			pm.t.Fatalf("failed to unmarshal params: %v", err)
		}
		resp := Response{
			JSONRPC: JSONRPCVersion2,
			ID:      req.ID,
			Result: &CallToolResult{
				Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf("proxy result for: %s", params.Name)}},
			},
		}
		respBytes, _ := json.Marshal(resp)
		pm.sseCh <- sseOutgoing{data: string(respBytes)}

	case "ping":
		resp := Response{JSONRPC: JSONRPCVersion2, ID: req.ID, Result: map[string]string{"status": "ok"}}
		respBytes, _ := json.Marshal(resp)
		pm.sseCh <- sseOutgoing{data: string(respBytes)}

	case "notifications/initialized":
	default:
		resp := Response{
			JSONRPC: JSONRPCVersion2,
			ID:      req.ID,
			Error:   &Error{Code: ErrCodeMethodNotFound, Message: "unknown method"},
		}
		respBytes, _ := json.Marshal(resp)
		pm.sseCh <- sseOutgoing{data: string(respBytes)}
	}

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, "Accepted")
}

func TestHTTPTransport_ProxyMode_Initialize(t *testing.T) {
	proxy := newProxyMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    proxy.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	params := InitializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    ClientCapabilities{},
		ClientInfo:      ImplementationInfo{Name: "nenya-test", Version: "0.0.1"},
	}

	resp, err := transport.SendRequest(t.Context(), "initialize", params)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	if serverInfo, ok := resultMap["serverInfo"].(map[string]any); ok {
		if name, _ := serverInfo["name"].(string); name != "proxy-mock" {
			t.Fatalf("serverInfo.name = %q, want %q", name, "proxy-mock")
		}
	}
}

func TestHTTPTransport_ProxyMode_ToolsList(t *testing.T) {
	proxy := newProxyMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    proxy.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	resp, err := transport.SendRequest(t.Context(), "tools/list", nil)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	toolsRaw, ok := resultMap["tools"].([]any)
	if !ok {
		t.Fatal("expected tools array in result")
	}
	if len(toolsRaw) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(toolsRaw))
	}
}

func TestHTTPTransport_ProxyMode_ToolCall(t *testing.T) {
	proxy := newProxyMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    proxy.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	params := CallToolParams{
		Name:      "proxy_tool",
		Arguments: map[string]any{"query": "test"},
	}

	resp, err := transport.SendRequest(t.Context(), "tools/call", params)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}

	content, ok := resultMap["content"].([]any)
	if !ok {
		t.Fatal("expected content array in result")
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
}

func TestHTTPTransport_ProxyMode_Ping(t *testing.T) {
	proxy := newProxyMockMCPServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:    proxy.server.URL + "/sse",
		Logger: newTestLogger(),
	})

	if err := transport.Connect(t.Context()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer transport.Close()

	resp, err := transport.SendRequest(t.Context(), "ping", nil)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}
	if status, _ := resultMap["status"].(string); status != "ok" {
		t.Fatalf("status = %q, want %q", status, "ok")
	}
}
