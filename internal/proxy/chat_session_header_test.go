package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/testutil"
)

// invalidSessionValues are client-supplied header values that
// validSessionHeaderValue rejects and ensureOpencodeSessionHeader must
// replace or drop.
var invalidSessionValues = []string{
	"bad\x00value",
	strings.Repeat("a", 257),
	"sesi\xc3\xb3n",
	"\t",
}

// newSessionCaptureUpstream starts an SSE upstream that records the
// X-Opencode-Session header of each request. The returned getter reads the
// captures under the server's lock.
func newSessionCaptureUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestBuildUpstreamRequest_ForwardsOpencodeSession(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{
			UserAgent: "nenya-test/1.0",
		},
	}
	providers := map[string]*config.Provider{
		"test": {
			Name:      "test",
			URL:       "https://api.test.com/v1/chat/completions",
			BaseURL:   "https://api.test.com",
			AuthStyle: "none",
		},
	}
	gw := &gateway.NenyaGateway{
		Config:    cfg,
		Secrets:   &config.SecretsConfig{ClientToken: "client-token"},
		Providers: providers,
		Logger:    infra.SetupLogger(false),
	}

	src := http.Header{}
	src.Set("X-Opencode-Session", "client-session-abc")
	src.Set("X-Custom-Not-Allowed", "leak")

	prx := &Proxy{}
	upstreamReq, err := prx.buildUpstreamRequest(gw, context.Background(), "POST", "https://api.test.com/v1/chat/completions", []byte(`{}`), "test", "", "", src)
	if err != nil {
		t.Fatalf("failed to build upstream request: %v", err)
	}

	if got := upstreamReq.Header.Get("X-Opencode-Session"); got != "client-session-abc" {
		t.Errorf("expected X-Opencode-Session 'client-session-abc', got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Custom-Not-Allowed"); got != "" {
		t.Errorf("expected X-Custom-Not-Allowed to be stripped, got %q", got)
	}
}

func TestHandleChatCompletions_SynthesizesOpencodeSession(t *testing.T) {
	upstream, captures := newSessionCaptureUpstream(t)
	p := newChatProxy(t, upstream.URL)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"first question"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	got := captures()
	if len(got) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(got))
	}
	if !strings.HasPrefix(got[0], "nenya-") {
		t.Fatalf("expected synthesized session ID with 'nenya-' prefix, got %q", got[0])
	}
	if len(got[0]) != len("nenya-")+16 {
		t.Errorf("expected synthesized session ID 'nenya-'+16 hex chars, got %q", got[0])
	}
}

func TestHandleChatCompletions_ClientSessionHeaderWins(t *testing.T) {
	upstream, captures := newSessionCaptureUpstream(t)
	p := newChatProxy(t, upstream.URL)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"first question"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("X-Opencode-Session", "client-session-abc")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	got := captures()
	if len(got) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(got))
	}
	if got[0] != "client-session-abc" {
		t.Errorf("expected client-supplied session ID to be preserved, got %q", got[0])
	}
}

func TestHandleChatCompletions_OpencodeSessionStablePerConversation(t *testing.T) {
	upstream, captures := newSessionCaptureUpstream(t)
	p := newChatProxy(t, upstream.URL)

	bodies := []string{
		// Conversation A, turn 1.
		`{"model":"test-agent","messages":[{"role":"user","content":"first"}]}`,
		// Conversation A, turn 2: same first user message, longer history.
		`{"model":"test-agent","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`,
		// Conversation B: different first user message.
		`{"model":"test-agent","messages":[{"role":"user","content":"different first"}]}`,
	}
	for _, body := range bodies {
		req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	}

	got := captures()
	if len(got) != 3 {
		t.Fatalf("expected 3 upstream requests, got %d", len(got))
	}
	for i, id := range got {
		if !strings.HasPrefix(id, "nenya-") {
			t.Fatalf("request %d: expected synthesized session ID, got %q", i, id)
		}
	}
	if got[0] != got[1] {
		t.Errorf("expected stable session ID across turns of the same conversation, got %q and %q", got[0], got[1])
	}
	if got[0] == got[2] {
		t.Errorf("expected distinct session IDs across conversations, got %q for both", got[0])
	}
}

func TestHandleMessages_SynthesizesOpencodeSession(t *testing.T) {
	upstream, captures := newSessionCaptureUpstream(t)
	p := newChatProxy(t, upstream.URL)

	body := `{"model":"test-agent","max_tokens":16,"messages":[{"role":"user","content":"first question"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	got := captures()
	if len(got) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(got))
	}
	if !strings.HasPrefix(got[0], "nenya-") {
		t.Fatalf("expected synthesized session ID with 'nenya-' prefix on /v1/messages, got %q", got[0])
	}
}

func TestHandleMessages_ClientSessionHeaderWins(t *testing.T) {
	upstream, captures := newSessionCaptureUpstream(t)
	p := newChatProxy(t, upstream.URL)

	body := `{"model":"test-agent","max_tokens":16,"messages":[{"role":"user","content":"first question"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/messages", body)
	req.Header.Set("X-Opencode-Session", "client-session-abc")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	got := captures()
	if len(got) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(got))
	}
	if got[0] != "client-session-abc" {
		t.Errorf("expected client-supplied session ID to be preserved on /v1/messages, got %q", got[0])
	}
}

func TestHandleMessages_InvalidClientValueReplaced(t *testing.T) {
	for _, value := range invalidSessionValues {
		upstream, captures := newSessionCaptureUpstream(t)
		p := newChatProxy(t, upstream.URL)

		body := `{"model":"test-agent","max_tokens":16,"messages":[{"role":"user","content":"first question"}]}`
		req := testutil.NewTestRequest(t, http.MethodPost, "/v1/messages", body)
		req.Header.Set("X-Opencode-Session", value)
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)
		testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

		got := captures()
		if len(got) != 1 {
			t.Fatalf("value %q: expected 1 upstream request, got %d", value, len(got))
		}
		if !strings.HasPrefix(got[0], "nenya-") {
			t.Errorf("value %q: expected unusable client value to be replaced on /v1/messages, got %q", value, got[0])
		}
	}
}

func TestEnsureOpencodeSessionHeader_NoDerivableKey(t *testing.T) {
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{
		Config: config.Config{},
		Logger: logger,
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req := &chatRequest{ModelName: "test-agent", Payload: map[string]any{}}

	ensureOpencodeSessionHeader(gw, r, req)

	if got := r.Header.Get("X-Opencode-Session"); got != "" {
		t.Errorf("expected no header without a derivable session key, got %q", got)
	}
}

func TestEnsureOpencodeSessionHeader_InvalidClientValueReplaced(t *testing.T) {
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{
		Config: config.Config{},
		Logger: logger,
	}
	for _, value := range invalidSessionValues {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Opencode-Session", value)
		req := &chatRequest{ModelName: "test-agent", Payload: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "first"}},
		}}

		ensureOpencodeSessionHeader(gw, r, req)

		got := r.Header.Get("X-Opencode-Session")
		if !strings.HasPrefix(got, "nenya-") {
			t.Errorf("value %q: expected unusable client value to be replaced with synthesized ID, got %q", value, got)
		}
		if !validSessionHeaderValue(got) {
			t.Errorf("value %q: expected replacement value to be a valid header value, got %q", value, got)
		}
	}
}

func TestEnsureOpencodeSessionHeader_InvalidValueDroppedWithoutKey(t *testing.T) {
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{
		Config: config.Config{},
		Logger: logger,
	}
	for _, value := range invalidSessionValues {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Opencode-Session", value)
		req := &chatRequest{ModelName: "test-agent", Payload: map[string]any{}}

		ensureOpencodeSessionHeader(gw, r, req)

		if got := r.Header.Get("X-Opencode-Session"); got != "" {
			t.Errorf("value %q: expected unusable client value to be dropped when no key is derivable, got %q", value, got)
		}
	}
}

func TestValidSessionHeaderValue(t *testing.T) {
	exactly256 := strings.Repeat("a", 256)
	over256 := strings.Repeat("a", 257)
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty", "", false},
		{"simple", "client-session-abc", true},
		{"tab allowed", "a\tb", true},
		{"tab only", "\t", false},
		{"spaces only", "   ", false},
		{"exactly 256 chars", exactly256, true},
		{"257 chars", over256, false},
		{"control byte", "bad\x00value", false},
		{"newline", "bad\nvalue", false},
		{"obs-text rejected", "sesi\xc3\xb3n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validSessionHeaderValue(tt.value); got != tt.want {
				t.Errorf("validSessionHeaderValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
