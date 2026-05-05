package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMistralAdapter_MutateRequest_EmptyBody(t *testing.T) {
	a := NewMistralAdapter()
	out, err := a.MutateRequest(nil, "mistral-large", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestMistralAdapter_MutateRequest_Identity(t *testing.T) {
	a := NewMistralAdapter()
	body := []byte(`{"model":"mistral-large","messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "mistral-large", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestMistralAdapter_MutateRequest_KeepsAutoToolChoice(t *testing.T) {
	a := NewMistralAdapter()
	body := []byte(`{"model":"mistral-large","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`)
	out, err := a.MutateRequest(body, "mistral-large", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, ok := m["tool_choice"]; !ok {
		t.Error("tool_choice 'auto' should have been kept (AutoToolChoice enabled)")
	}
}

func TestMistralAdapter_InjectAuth(t *testing.T) {
	a := NewMistralAdapter()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("expected 'Bearer test-key', got %q", got)
	}
}

func TestMistralAdapter_InjectAuth_EmptyKey(t *testing.T) {
	a := NewMistralAdapter()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "")
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestMistralAdapter_MutateResponse(t *testing.T) {
	a := NewMistralAdapter()
	body := []byte(`{"id":"resp1","choices":[{"message":{"content":"hi"}}]}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestMistralAdapter_NormalizeError(t *testing.T) {
	a := NewMistralAdapter()
	tests := []struct {
		code int
		body string
		want ErrorClass
	}{
		{429, "", ErrorRateLimited},
		{500, "", ErrorRetryable},
		{400, `{"error":"context_length_exceeded"}`, ErrorRetryable},
		{400, `{"error":"invalid_input"}`, ErrorPermanent},
		{403, "", ErrorPermanent},
	}
	for _, tt := range tests {
		got := a.NormalizeError(tt.code, []byte(tt.body))
		if got != tt.want {
			t.Errorf("NormalizeError(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
		}
	}
}
