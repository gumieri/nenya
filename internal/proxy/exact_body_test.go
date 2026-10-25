package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/internal/testutil"
)

// TestExactMaxBodyAccepted pins the AUD-23 resolution: a buffered
// (non-streaming) request body of EXACTLY max_body_bytes (default 10 MiB)
// must be accepted — the limit is INCLUSIVE end to end. On Go 1.26
// http.MaxBytesReader passes an exact-n body (verified against the runtime),
// and every body-reading route (chat scoping, validateChatRequest,
// responses/embeddings/passthrough/files/systemone) defers to it. If this
// test ever 413s, someone introduced an off-by-one in the body path.
func TestExactMaxBodyAccepted(t *testing.T) {
	p, _ := newTestProxy(t)

	// Padding chosen so the final body length is exactly 10 MiB (the +1
	// covers the closing quote of the content string). Space-separated words
	// keep the BPE tokenizer fast — one repeated byte is pathological for
	// the merge loop.
	const limit = 10 << 20
	skeleton := `{"model":"test-agent","messages":[{"role":"user","content":"` + strings.Repeat("a", 1) + `"}]}`
	pad := limit - len(skeleton) + 1
	if pad < 0 {
		t.Fatalf("skeleton already over the limit: %d", len(skeleton))
	}
	body := `{"model":"test-agent","messages":[{"role":"user","content":"` + strings.Repeat("w ", pad/2) + `"}]}`
	if len(body) != limit {
		t.Fatalf("fixture misbuilt: len=%d want %d", len(body), limit)
	}

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	t.Logf("status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body of exactly max_body_bytes (%d) was rejected as over-limit (AUD-23)", limit)
	}
}
