package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newHandshakeServer returns a test MCP server that immediately completes
// the SSE handshake (sends the endpoint event) and nothing else.
func newHandshakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
	}))
}

// retryTestConfig returns a transport config for handshake-server tests.
func retryTestConfig(url string) TransportConfig {
	return TransportConfig{
		URL:            url,
		ConnectTimeout: 5 * time.Second,
		Logger:         newTestLogger(),
	}
}

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

	transport := NewHTTPTransport(retryTestConfig(server.URL))
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestConnect_FirstAttemptSucceeds(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
	}))
	defer server.Close()

	transport := NewHTTPTransport(retryTestConfig(server.URL))
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestConnect_RejectsAfterClose(t *testing.T) {
	server := newHandshakeServer(t)
	defer server.Close()

	transport := NewHTTPTransport(retryTestConfig(server.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("first connect failed: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if err := transport.Connect(ctx); !errors.Is(err, ErrTransportClosed) {
		t.Errorf("expected ErrTransportClosed after Close, got: %v", err)
	}
}

func TestConnect_RejectsSecondConnection(t *testing.T) {
	server := newHandshakeServer(t)
	defer server.Close()

	transport := NewHTTPTransport(retryTestConfig(server.URL))
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("first connect failed: %v", err)
	}
	if err := transport.Connect(ctx); !errors.Is(err, ErrTransportAlreadyConnected) {
		t.Errorf("expected ErrTransportAlreadyConnected on second Connect, got: %v", err)
	}
}

func TestConnect_ConcurrentSecondCallRejected(t *testing.T) {
	server := newHandshakeServer(t)
	defer server.Close()

	transport := NewHTTPTransport(retryTestConfig(server.URL))
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Two concurrent Connect calls: exactly one wins, the loser is rejected
	// without panicking on a double doneCh close.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := range errs {
		go func(i int) {
			defer wg.Done()
			errs[i] = transport.Connect(ctx)
		}(i)
	}
	wg.Wait()

	succeeded, rejected := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrTransportAlreadyConnected):
			rejected++
		default:
			t.Fatalf("unexpected concurrent connect error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Errorf("expected 1 success + 1 rejection, got %d succeeded / %d rejected", succeeded, rejected)
	}
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
		Logger:         newTestLogger(),
	}
	transport := NewHTTPTransport(cfg)
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("connect failed: %v", err)
	}

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

func TestKeepalive_RecoversAfterTransientFailure(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Complete the handshake, then hold the SSE stream open for the
			// lifetime of the test connection.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"endpoint\":\"/message\"}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case http.MethodPost:
			// The first 2 keepalive pings (2 retry attempts each) fail;
			// later ones succeed so a subsequent tick restores readiness.
			if posts.Add(1) <= 4 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()

	cfg := retryTestConfig(server.URL)
	cfg.KeepAliveInterval = 25 * time.Millisecond
	cfg.RequestTimeout = 2 * time.Second
	transport := NewHTTPTransport(cfg)
	defer func() { _ = transport.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("connect failed: %v", err)
	}

	// Wait for the failed pings to mark the transport unhealthy. Each
	// failed ping spans ~1s (retry backoff), so allow ample time.
	sawUnhealthy := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !transport.Ready() {
			sawUnhealthy = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawUnhealthy {
		t.Fatal("transport never became unhealthy after failed keepalive ping")
	}

	// Wait for the next successful ping to restore readiness.
	sawRecovered := false
	for time.Now().Before(deadline) {
		if transport.Ready() {
			sawRecovered = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawRecovered {
		t.Fatal("transport never recovered readiness after transient ping failure")
	}
}

func TestConnect_HandshakeStallTimesOut(t *testing.T) {
	// Server completes the SSE headers but never sends the endpoint event:
	// Connect must fail within ConnectTimeout instead of hanging forever.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // stall until the client gives up
	}))
	defer server.Close()

	cfg := retryTestConfig(server.URL)
	cfg.ConnectTimeout = 150 * time.Millisecond
	transport := NewHTTPTransport(cfg)
	defer func() { _ = transport.Close() }()

	start := time.Now()
	err := transport.Connect(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected connect to fail on stalled handshake, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("connect took %v, expected failure within ~ConnectTimeout", elapsed)
	}
	if transport.Ready() {
		t.Error("transport must not be ready after failed handshake")
	}
}
