package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnect_RetryOnNetworkError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
	}))
	defer server.Close()

	cfg := TransportConfig{
		URL:            server.URL,
		ConnectTimeout: 5 * time.Second,
		Logger:         slog.Default(),
	}
	transport := NewHTTPTransport(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
	transport.Close()
}

func TestConnect_FirstAttemptSucceeds(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
	}))
	defer server.Close()

	cfg := TransportConfig{
		URL:            server.URL,
		ConnectTimeout: 5 * time.Second,
		Logger:         slog.Default(),
	}
	transport := NewHTTPTransport(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts.Load())
	}
	transport.Close()
}

func TestSendRequest_RetryOnNetworkError(t *testing.T) {
	var postAttempts atomic.Int32
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" || r.URL.Path == "/sse/" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-done
			return
		}
		n := postAttempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	}))
	defer server.Close()
	defer close(done)

	cfg := TransportConfig{
		URL:            server.URL,
		ConnectTimeout: 5 * time.Second,
		RequestTimeout: 10 * time.Second,
		Logger:         slog.Default(),
	}
	transport := NewHTTPTransport(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer transport.Close()

	if transport.SessionEndpoint() == "" {
		t.Fatalf("expected session endpoint, got empty")
	}

	resp, err := transport.SendRequest(ctx, "test_method", nil)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}
	resultStr, ok := resp.Result.(string)
	if !ok || resultStr != "ok" {
		t.Errorf("expected result \"ok\", got %v (%T)", resp.Result, resp.Result)
	}
	if postAttempts.Load() != 2 {
		t.Errorf("expected 2 POST attempts, got %d", postAttempts.Load())
	}
}
