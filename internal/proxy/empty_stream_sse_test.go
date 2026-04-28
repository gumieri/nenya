package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteEmptyStreamSSE(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	rec := httptest.NewRecorder()
	p.writeEmptyStreamSSE(p.Gateway(), rec)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected SSE error type, got: %s", body)
	}
	if !strings.Contains(body, `"empty_response"`) {
		t.Fatalf("expected empty_response code, got: %s", body)
	}
	if !strings.Contains(body, `"empty upstream SSE"`) {
		t.Fatalf("expected error message, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]\n") {
		t.Fatalf("expected SSE [DONE] sentinel, got: %s", body)
	}

	sseLines := strings.Split(strings.TrimSpace(body), "\n")
	if len(sseLines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(sseLines))
	}
	for _, line := range sseLines {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("all non-empty lines must start with 'data: ', got: %s", line)
		}
	}
}

func TestWriteEmptyStreamSSE_Flush(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	rec := httptest.NewRecorder()
	p.writeEmptyStreamSSE(p.Gateway(), rec)

	if !rec.Flushed {
		t.Fatal("expected response to be flushed")
	}
}
