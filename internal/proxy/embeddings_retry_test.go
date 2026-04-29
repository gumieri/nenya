package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHandleEmbeddings_RetryOnNetworkError(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]interface{}{"embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()
	gw.Config.Governance.MaxRetryAttempts = 5
	gw.Providers["deepseek"].MaxRetryAttempts = 3

	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestHandleEmbeddings_NoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "bad request"})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()
	gw.Providers["deepseek"].MaxRetryAttempts = 3

	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (passthrough) for upstream 400, got %d", rec.Code)
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt (no retry on 400), got %d", attempts.Load())
	}
}

func TestHandleResponses_RetryOnNetworkError(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp-123"})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()
	gw.Config.Governance.MaxRetryAttempts = 5
	gw.Providers["deepseek"].MaxRetryAttempts = 2

	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestHandleResponses_NoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()
	gw.Providers["deepseek"].MaxRetryAttempts = 3

	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (passthrough) for upstream 400, got %d", rec.Code)
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts.Load())
	}
}
