package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMem0Search(t *testing.T) {
	wantResults := []MemoryResult{
		{ID: "abc-123", Memory: "User prefers Go for backend services", Score: 0.92},
		{ID: "def-456", Memory: "Project uses PostgreSQL", Score: 0.85},
	}
	respBody, _ := json.Marshal(searchResponse{Results: wantResults})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		var req searchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Query != "Go backend" {
			t.Errorf("unexpected query: %s", req.Query)
		}
		if req.UserID != "user-1" {
			t.Errorf("unexpected user_id: %s", req.UserID)
		}
		if req.TopK != 10 {
			t.Errorf("expected default top_k=10, got %d", req.TopK)
		}
		if req.Threshold != 0.3 {
			t.Errorf("expected default threshold=0.3, got %f", req.Threshold)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	results, err := client.Search(context.Background(), "Go backend")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != "abc-123" || results[0].Memory != "User prefers Go for backend services" {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if results[1].Score != 0.85 {
		t.Errorf("unexpected second result score: %f", results[1].Score)
	}
}

func TestMem0SearchCustomParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.TopK != 5 {
			t.Errorf("expected top_k=5, got %d", req.TopK)
		}
		if req.Threshold != 0.7 {
			t.Errorf("expected threshold=0.7, got %f", req.Threshold)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(searchResponse{Results: nil})
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:       srv.URL,
		UserID:    "user-1",
		TopK:      5,
		Threshold: 0.7,
	}, nil)

	results, err := client.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestMem0Add(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memories" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		var req addRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.UserID != "user-1" {
			t.Errorf("unexpected user_id: %s", req.UserID)
		}
		if len(req.Messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(req.Messages))
		}
		if req.Messages[0].Role != "user" || req.Messages[0].Content != "hello" {
			t.Errorf("unexpected first message: %+v", req.Messages[0])
		}
		if req.Messages[1].Role != "assistant" || req.Messages[1].Content != "hi there" {
			t.Errorf("unexpected second message: %+v", req.Messages[1])
		}

		resp := addResponse{
			Results: []addResult{
				{ID: "mem-1", Memory: "User said hello", Event: "ADD"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	messages := []AddMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	err := client.Add(context.Background(), messages)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}
}

func TestMem0AddEmptyMessages(t *testing.T) {
	client := NewMem0Client(MemoryConfig{URL: "http://localhost:9999"}, nil)
	err := client.Add(context.Background(), nil)
	if err != nil {
		t.Errorf("expected nil error for empty messages, got: %v", err)
	}
}

func TestMem0SearchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	_, err := client.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestMem0AuthHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(searchResponse{Results: nil})
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		APIKey: "secret-key-123",
		UserID: "user-1",
	}, nil)

	_, err := client.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if gotKey != "secret-key-123" {
		t.Errorf("expected X-API-Key header 'secret-key-123', got %q", gotKey)
	}
}

func TestMem0NoAuthHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(searchResponse{Results: nil})
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	_, err := client.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if gotKey != "" {
		t.Errorf("expected no X-API-Key header, got %q", gotKey)
	}
}

func TestFormatMemoryContext(t *testing.T) {
	t.Run("empty results", func(t *testing.T) {
		result := FormatMemoryContext(nil)
		if result != "" {
			t.Errorf("expected empty string, got %q", result)
		}
	})

	t.Run("with results", func(t *testing.T) {
		results := []MemoryResult{
			{Memory: "User likes Go"},
			{Memory: "Project uses k8s"},
		}
		result := FormatMemoryContext(results)
		expected := "[Relevant memory context]\n- User likes Go\n- Project uses k8s\n"
		if result != expected {
			t.Errorf("unexpected format:\n%s\nexpected:\n%s", result, expected)
		}
	})

	t.Run("skips empty memory", func(t *testing.T) {
		results := []MemoryResult{
			{Memory: "User likes Go"},
			{Memory: ""},
			{Memory: "Project uses k8s"},
		}
		result := FormatMemoryContext(results)
		expected := "[Relevant memory context]\n- User likes Go\n- Project uses k8s\n"
		if result != expected {
			t.Errorf("unexpected format:\n%s\nexpected:\n%s", result, expected)
		}
	})
}

func TestTruncateLog(t *testing.T) {
	t.Run("short string", func(t *testing.T) {
		result := truncateLog("hello", 10)
		if result != "hello" {
			t.Errorf("expected 'hello', got %q", result)
		}
	})
	t.Run("exact length", func(t *testing.T) {
		result := truncateLog("hello", 5)
		if result != "hello" {
			t.Errorf("expected 'hello', got %q", result)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		result := truncateLog("hello world", 5)
		if result != "hello..." {
			t.Errorf("expected 'hello...', got %q", result)
		}
	})
	t.Run("multi-byte runes", func(t *testing.T) {
		result := truncateLog("日本語テスト", 3)
		if result != "日本語..." {
			t.Errorf("expected '日本語...', got %q", result)
		}
	})
}

func TestContentBuilder(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		cb := NewContentBuilder()
		if cb.Build() != "" {
			t.Errorf("expected empty string, got %q", cb.Build())
		}
	})
	t.Run("single add", func(t *testing.T) {
		cb := NewContentBuilder()
		cb.AddContent("hello")
		if cb.Build() != "hello" {
			t.Errorf("expected 'hello', got %q", cb.Build())
		}
	})
	t.Run("multiple adds", func(t *testing.T) {
		cb := NewContentBuilder()
		cb.AddContent("hello")
		cb.AddContent(" ")
		cb.AddContent("world")
		if cb.Build() != "hello world" {
			t.Errorf("expected 'hello world', got %q", cb.Build())
		}
	})
}

func TestMem0SearchContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(searchResponse{Results: nil})
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Search(ctx, "test")
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

func TestMem0SearchMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	_, err := client.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for malformed JSON response, got nil")
	}
}

func TestMem0AddError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	err := client.Add(context.Background(), []AddMessage{{Role: "assistant", Content: "test"}})
	if err == nil {
		t.Fatal("expected error for 503 response, got nil")
	}
}

func TestMem0AddContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(addResponse{Results: nil})
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.Add(ctx, []AddMessage{{Role: "assistant", Content: "test"}})
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

func TestMem0AddMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, nil)

	err := client.Add(context.Background(), []AddMessage{{Role: "assistant", Content: "test"}})
	if err == nil {
		t.Fatal("expected error for malformed JSON response, got nil")
	}
}

func TestMem0AddDebugLogging(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := addResponse{
			Results: []addResult{
				{ID: "mem-1", Memory: "Important fact about user", Event: "ADD"},
				{ID: "mem-2", Memory: "Ignored update", Event: "NONE"},
			},
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewMem0Client(MemoryConfig{
		URL:    srv.URL,
		UserID: "user-1",
	}, logger)

	err := client.Add(context.Background(), []AddMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "mem-1") {
		t.Errorf("expected debug log for ADD event, got: %s", logOutput)
	}
	if strings.Contains(logOutput, "NONE") {
		t.Errorf("expected no debug log for NONE event, got: %s", logOutput)
	}
}
