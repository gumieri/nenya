package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewTestRequest creates a new HTTP request with common defaults for testing.
// Sets the Authorization header and content type for JSON requests.
func NewTestRequest(t *testing.T, method, url string, body interface{}) *http.Request {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			bodyReader = strings.NewReader(v)
		case []byte:
			bodyReader = bytes.NewReader(v)
		default:
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("failed to marshal request body: %v", err)
			}
			bodyReader = bytes.NewReader(b)
		}
	}

	req := httptest.NewRequest(method, url, bodyReader)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// AssertResponseStatusCode asserts that the response has the expected status code.
func AssertResponseStatusCode(t *testing.T, rec *httptest.ResponseRecorder, expected int) {
	t.Helper()

	if rec.Code != expected {
		t.Errorf("expected status code %d, got %d", expected, rec.Code)
	}
}
