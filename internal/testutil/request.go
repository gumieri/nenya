package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// NewTestResponseRecorder creates a new response recorder with common defaults.
func NewTestResponseRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// ReadResponseBody reads and returns the response body from a recorder.
func ReadResponseBody(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()

	body, err := ioutil.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return body
}

// AssertResponseStatusCode asserts that the response has the expected status code.
func AssertResponseStatusCode(t *testing.T, rec *httptest.ResponseRecorder, expected int) {
	t.Helper()

	if rec.Code != expected {
		t.Errorf("expected status code %d, got %d", expected, rec.Code)
	}
}

// AssertResponseContentType asserts that the response has the expected content type.
func AssertResponseContentType(t *testing.T, rec *httptest.ResponseRecorder, expected string) {
	t.Helper()

	actual := rec.Header().Get("Content-Type")
	if actual != expected {
		t.Errorf("expected content type %q, got %q", expected, actual)
	}
}

// AssertResponseHeader asserts that the response has the expected header value.
func AssertResponseHeader(t *testing.T, rec *httptest.ResponseRecorder, key, expected string) {
	t.Helper()

	actual := rec.Header().Get(key)
	if actual != expected {
		t.Errorf("expected header %q value %q, got %q", key, expected, actual)
	}
}

// AssertResponseBodyContains asserts that the response body contains the expected string.
func AssertResponseBodyContains(t *testing.T, rec *httptest.ResponseRecorder, expected string) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if !bytes.Contains(body, []byte(expected)) {
		t.Errorf("expected response body to contain %q, got %q", expected, string(body))
	}
}

// AssertResponseBodyEquals asserts that the response body equals the expected string.
func AssertResponseBodyEquals(t *testing.T, rec *httptest.ResponseRecorder, expected string) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if string(body) != expected {
		t.Errorf("expected response body %q, got %q", expected, string(body))
	}
}

// AssertResponseBodyJSON asserts that the response body is valid JSON.
func AssertResponseBodyJSON(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	var js map[string]interface{}
	if err := json.Unmarshal(body, &js); err != nil {
		t.Errorf("expected valid JSON, got error: %v, body: %q", err, string(body))
	}
}

// AssertResponseBodyJSONField asserts that the JSON response body has a specific field with expected value.
func AssertResponseBodyJSONField(t *testing.T, rec *httptest.ResponseRecorder, field string, expected interface{}) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	var js map[string]interface{}
	if err := json.Unmarshal(body, &js); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}

	actual, ok := js[field]
	if !ok {
		t.Errorf("expected JSON field %q not found", field)
		return
	}

	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("expected JSON field %q value %v, got %v", field, expected, actual)
	}
}

// AssertResponseSSETokens asserts that the SSE response contains the expected number of tokens.
func AssertResponseSSETokens(t *testing.T, rec *httptest.ResponseRecorder, expectedTokens int) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	lines := strings.Split(string(body), "\n")
	tokenCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			tokenCount++
		}
	}

	if tokenCount != expectedTokens {
		t.Errorf("expected %d tokens in SSE response, got %d", expectedTokens, tokenCount)
	}
}

// AssertResponseSSEContains asserts that the SSE response contains the expected string.
func AssertResponseSSEContains(t *testing.T, rec *httptest.ResponseRecorder, expected string) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if !bytes.Contains(body, []byte(expected)) {
		t.Errorf("expected SSE response to contain %q, got %q", expected, string(body))
	}
}

// AssertResponseSSEEquals asserts that the SSE response equals the expected string.
func AssertResponseSSEEquals(t *testing.T, rec *httptest.ResponseRecorder, expected string) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if string(body) != expected {
		t.Errorf("expected SSE response %q, got %q", expected, string(body))
	}
}

// AssertResponseSSEValid asserts that the SSE response is valid.
func AssertResponseSSEValid(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "id: ") && !strings.HasPrefix(line, "event: ") && line != "[DONE]" {
			t.Errorf("invalid SSE line: %q", line)
		}
	}
}

// AssertResponseSSEComplete asserts that the SSE response ends with [DONE].
func AssertResponseSSEComplete(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if !strings.HasSuffix(string(body), "[DONE]\n") {
		t.Errorf("expected SSE response to end with [DONE]\n, got %q", string(body))
	}
}

// AssertResponseSSENoContent asserts that the SSE response contains no content (only [DONE]).
func AssertResponseSSENoContent(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	if string(body) != "[DONE]\n" {
		t.Errorf("expected SSE response with no content, got %q", string(body))
	}
}

// AssertResponseSSETokensInContent asserts that the SSE content contains the expected number of tokens.
func AssertResponseSSETokensInContent(t *testing.T, rec *httptest.ResponseRecorder, expectedTokens int) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	content := ""
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			content += strings.TrimPrefix(line, "data: ")
		}
	}

	tokens := strings.Fields(content)
	if len(tokens) != expectedTokens {
		t.Errorf("expected %d tokens in SSE content, got %d", expectedTokens, len(tokens))
	}
}

// AssertResponseSSETokensInJSONContent asserts that the JSON SSE content contains the expected number of tokens.
func AssertResponseSSETokensInJSONContent(t *testing.T, rec *httptest.ResponseRecorder, expectedTokens int) {
	t.Helper()

	body := ReadResponseBody(t, rec)
	content := ""
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			content += strings.TrimPrefix(line, "data: ")
		}
	}

	var js map[string]interface{}
	if err := json.Unmarshal([]byte(content), &js); err != nil {
		t.Fatalf("expected valid JSON in SSE content, got error: %v", err)
	}

	choices, ok := js["choices"].([]interface{})
	if !ok {
		t.Fatalf("expected choices array in JSON content")
	}

	tokenCount := 0
	for _, choice := range choices {
		choiceMap, ok := choice.(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choiceMap["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := delta["content"].(string)
		if !ok {
			continue
		}
		tokens := strings.Fields(content)
		tokenCount += len(tokens)
	}

	if tokenCount != expectedTokens {
		t.Errorf("expected %d tokens in JSON SSE content, got %d", expectedTokens, tokenCount)
	}
}
