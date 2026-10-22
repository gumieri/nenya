package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// noSpaceSSEServer is a mock MCP server whose SSE stream uses the no-space
// field spelling real (non-compliant but common) servers emit:
//
//	event:endpoint
//	data:{...}
//
// NENYA-197's tolerance grammar (an optional single space after the field
// name) applies to every SSE consumer, the MCP transport included. Pre-fix,
// the transport's readers required the canonical "data: "/"event: " prefixes:
// the endpoint event was skipped (Connect timed out) and JSON-RPC responses
// pushed on the stream were silently dropped (every request waited out its
// full RequestTimeout).
type noSpaceSSEServer struct {
	t      *testing.T
	server *httptest.Server
	// pushCh serializes every SSE write onto the /sse handler goroutine:
	// writing an http.ResponseWriter from two goroutines is a data race.
	pushCh chan Response
}

func newNoSpaceSSEServer(t *testing.T) *noSpaceSSEServer {
	ns := &noSpaceSSEServer{t: t, pushCh: make(chan Response, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", ns.handleSSE)
	mux.HandleFunc("/message", ns.handleMessage)
	ns.server = httptest.NewServer(mux)
	t.Cleanup(ns.server.Close)
	return ns
}

func (ns *noSpaceSSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		ns.t.Error("response writer not a flusher")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	endpoint := ns.server.URL + "/message"
	// No-space endpoint event: the Connect handshake must still parse it.
	_, _ = fmt.Fprintf(w, "event:endpoint\ndata:{\"endpoint\":%q}\n\n", endpoint)
	flusher.Flush()

	for {
		select {
		case resp := <-ns.pushCh:
			encoded, _ := json.Marshal(resp)
			_, _ = fmt.Fprintf(w, "data:%s\n\n", encoded)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// pushResponse delivers a JSON-RPC response over the SSE stream with the
// no-space data spelling. The write itself happens on the /sse handler
// goroutine (via pushCh) — never on the POST handler's. A timeout here is
// reported as a test error so a stalled handler is distinguishable from the
// transport dropping the frame.
func (ns *noSpaceSSEServer) pushResponse(resp Response) {
	select {
	case ns.pushCh <- resp:
	case <-time.After(5 * time.Second):
		ns.t.Error("pushResponse: the /sse handler did not consume the response within 5s")
	}
}

func (ns *noSpaceSSEServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}
	// SSE-response mode: the POST is acknowledged; the RPC response arrives
	// on the stream (this is the path sseReadLoop dispatches).
	w.WriteHeader(http.StatusAccepted)
	go func() {
		var result any
		switch req.Method {
		case "ping":
			result = map[string]string{"status": "ok"}
		default:
			result = map[string]string{"echo": req.Method}
		}
		ns.pushResponse(Response{JSONRPC: JSONRPCVersion2, ID: req.ID, Result: result})
	}()
}

func TestHTTPTransport_ToleratesNoSpaceSSEFields(t *testing.T) {
	ns := newNoSpaceSSEServer(t)

	transport := NewHTTPTransport(TransportConfig{
		URL:            ns.server.URL + "/sse",
		ConnectTimeout: 3 * time.Second,
		RequestTimeout: 3 * time.Second,
		Logger:         newTestLogger(),
	})
	defer func() { _ = transport.Close() }()

	ctx := context.Background()
	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("Connect must parse the no-space endpoint event: %v", err)
	}
	if !transport.Ready() {
		t.Fatal("transport not ready after a no-space endpoint event")
	}
	want := ns.server.URL + "/message"
	if got := transport.SessionEndpoint(); got != want {
		t.Fatalf("SessionEndpoint = %q, want %q", got, want)
	}

	// The headline defect: a JSON-RPC response pushed on the stream with the
	// no-space spelling was silently dropped by sseReadLoop, so every request
	// waited out its full RequestTimeout.
	resp, err := transport.SendRequest(ctx, "ping", nil)
	if err != nil {
		t.Fatalf("the no-space data line dropped the JSON-RPC response: %v", err)
	}
	if resp.Result == nil {
		t.Fatalf("expected a ping result, got %+v", resp)
	}
}

// Unit pin for the tolerance grammar: the field name is case-sensitive and
// at most ONE optional space separates it from the value.
func TestSSEFieldValue(t *testing.T) {
	cases := []struct {
		line  string
		field string
		value string
		ok    bool
	}{
		{"data: {\"x\":1}", "data", "{\"x\":1}", true},
		{"data:{\"x\":1}", "data", "{\"x\":1}", true},
		{"data:  {\"x\":1}", "data", " {\"x\":1}", true}, // second space belongs to the value
		{"DATA: x", "data", "", false},                   // case-sensitive
		{"datax", "data", "", false},                     // no colon
		{"event: endpoint", "event", "endpoint", true},   // event field
		{"event:endpoint", "event", "endpoint", true},    // no-space event
		{": keepalive", "data", "", false},               // comment line
		{"data:", "data", "", true},                      // empty value is a field
	}
	for _, tc := range cases {
		got, ok := sseFieldValue(tc.line, tc.field)
		if ok != tc.ok || (ok && got != tc.value) {
			t.Errorf("sseFieldValue(%q, %q) = (%q, %v), want (%q, %v)", tc.line, tc.field, got, ok, tc.value, tc.ok)
		}
	}
}

// An empty no-space data line inside an event block must be skipped, not
// terminate the scan: `event: x` + `data:` + `data: payload` still delivers
// "payload" (regression pin for the AUD-06 rework).
func TestReadMultiLineEventSkipsEmptyNoSpaceData(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("event: x\ndata:\ndata: payload\n\n"))
	if got := readMultiLineEvent(r); got != "payload" {
		t.Fatalf("readMultiLineEvent = %q, want %q (an empty no-space data line must not end the scan)", got, "payload")
	}
}
