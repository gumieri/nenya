package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nenya/internal/testutil"
)

// TestHasPathPrefix pins the segment-boundary matcher (AUD-20): a path is
// under a base only at a "/" boundary, so an undeclared look-alike such as
// /v1/responsesXYZ cannot reach a prefix route handler.
func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		path, base string
		want       bool
	}{
		{"/v1/responses", "/v1/responses", true},
		{"/v1/responses/", "/v1/responses", true},
		{"/v1/responses/resp_123", "/v1/responses", true},
		{"/v1/responsesXYZ", "/v1/responses", false},
		{"/v1/responsesXYZ/deep", "/v1/responses", false},
		{"/v1/filesX", "/v1/files", false},
		{"/v1/files/file_1/content", "/v1/files", true},
		{"/v1/batches", "/v1/batches", true},
		{"/proxy", "/proxy/", false},
		{"/proxy/openai/v1/chat/completions", "/proxy/", true},
		{"/v1/a2aXYZ", "/v1/a2a", false},
		{"/v1/moderationsX", "/v1/moderations", false},
	}
	for _, tc := range cases {
		if got := hasPathPrefix(tc.path, tc.base); got != tc.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", tc.path, tc.base, got, tc.want)
		}
	}
}

// TestResolveRouteRejectsLookalikePaths pins the routing consequence: an
// undeclared look-alike path must NOT resolve to the prefix handler (404 at
// the router), while genuine sub-paths still do.
func TestResolveRouteRejectsLookalikePaths(t *testing.T) {
	p := &Proxy{}
	cases := []struct {
		path string
		want bool // a handler was found
	}{
		{"/v1/responses", true},
		{"/v1/responses/resp_123", true},
		{"/v1/responses/resp_123/cancel", true},
		{"/v1/responsesXYZ", false},
		{"/v1/filesX", false},
		{"/v1/files/file_1", true},
		{"/v1/batchesXYZ", false},
		{"/v1/a2aXYZ", false},
	}
	for _, tc := range cases {
		if got := p.resolveRoute(tc.path) != nil; got != tc.want {
			t.Errorf("resolveRoute(%q) found handler = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestResponsesPathRejectsLookalike pins the canonicalizer: an undeclared
// /v1/responses-shaped path must not be accepted as an id-addressed operation
// (which would bypass the model-required check and forward it upstream).
func TestResponsesPathRejectsLookalike(t *testing.T) {
	if _, ok := canonicalResponsesPath("/v1/responsesXYZ"); ok {
		t.Fatal("/v1/responsesXYZ must not canonicalize as a responses path")
	}
	if _, ok := canonicalResponsesPath("/v1/responses/resp_1"); !ok {
		t.Fatal("a genuine id-addressed sub-path must still canonicalize")
	}
	if _, ok := canonicalResponsesPath("/v1/responses"); !ok {
		t.Fatal("the create endpoint must still canonicalize")
	}
}

// TestLookalikePathNotForwarded pins the end-to-end behavior: a request to an
// undeclared look-alike path is a 404 at the router rather than a proxied
// upstream call (the prefix handler is never entered, so nothing is
// dispatched).
func TestLookalikePathNotForwarded(t *testing.T) {
	p, authSeen, mu := newReloadWindowProxy(t)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/responsesXYZ", body)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("/v1/responsesXYZ must be 404, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls := len(*authSeen)
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("an undeclared look-alike path reached the upstream (%d calls)", calls)
	}
}
