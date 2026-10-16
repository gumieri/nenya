package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// newSessionHeaderGW builds the minimal gateway the session-header tests
// need (logger + empty config).
func newSessionHeaderGW() *gateway.NenyaGateway {
	return &gateway.NenyaGateway{
		Config: config.Config{},
		Logger: infra.SetupLogger(false),
	}
}

// derivableReq is a chatRequest whose session identity is derivable
// (first user message present), so synthesis is possible.
func derivableReq() *chatRequest {
	return &chatRequest{ModelName: "test-agent", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first"}},
	}}
}

// TestEnsureSessionHeaderSetIsOneIdentity pins NENYA-143: the namespaced
// pair, the legacy aliases, and the synthesized value are ONE logical
// session identity — precedence namespaced > legacy > synthesized, with
// both emitted names always agreeing.
func TestEnsureSessionHeaderSetIsOneIdentity(t *testing.T) {
	tests := []struct {
		name            string
		in              map[string]string
		req             *chatRequest
		wantSet         bool
		wantValue       string // when set
		wantSynthesized bool   // synthesized: value differs per run, assert prefix
	}{
		{name: "namespaced only", in: map[string]string{opencodeSessionIDHeader: "ns-1"}, req: derivableReq(), wantSet: true, wantValue: "ns-1"},
		{name: "legacy only", in: map[string]string{opencodeSessionHeader: "leg-1"}, req: derivableReq(), wantSet: true, wantValue: "leg-1"},
		{name: "both agree", in: map[string]string{opencodeSessionIDHeader: "same", opencodeSessionHeader: "same"}, req: derivableReq(), wantSet: true, wantValue: "same"},
		{name: "both differ namespaced wins", in: map[string]string{opencodeSessionIDHeader: "ns-wins", opencodeSessionHeader: "legacy-loses"}, req: derivableReq(), wantSet: true, wantValue: "ns-wins"},
		{name: "legacy alias X-Session-Id", in: map[string]string{legacySessionIDHeader: "alias-1"}, req: derivableReq(), wantSet: true, wantValue: "alias-1"},
		{name: "legacy affinity alias", in: map[string]string{legacySessionAffinityHeader: "affinity-1"}, req: derivableReq(), wantSet: true, wantValue: "affinity-1"},
		{name: "neither synthesizes", in: nil, req: derivableReq(), wantSet: true, wantSynthesized: true},
		{name: "unusable namespaced replaced by synthesis", in: map[string]string{opencodeSessionIDHeader: "bad\x01value"}, req: derivableReq(), wantSet: true, wantSynthesized: true},
		{name: "unusable namespaced dropped without key", in: map[string]string{opencodeSessionIDHeader: "bad\x01value"}, req: &chatRequest{ModelName: "test-agent", Payload: map[string]any{}}, wantSet: false},
		{name: "unusable legacy replaced by synthesis", in: map[string]string{opencodeSessionHeader: string(make([]byte, 300))}, req: derivableReq(), wantSet: true, wantSynthesized: true},
		// Mixed precedence: an unusable higher-precedence value must fall
		// through to a usable lower one — never synthesize over a valid
		// client id, never drop it.
		{name: "unusable namespaced falls through to valid legacy", in: map[string]string{opencodeSessionIDHeader: "bad\x01value", opencodeSessionHeader: "valid-legacy"}, req: derivableReq(), wantSet: true, wantValue: "valid-legacy"},
		{name: "unusable namespaced falls through without derivable key", in: map[string]string{opencodeSessionIDHeader: "bad\x01value", legacySessionIDHeader: "valid-alias"}, req: &chatRequest{ModelName: "test-agent", Payload: map[string]any{}}, wantSet: true, wantValue: "valid-alias"},
		{name: "unusable values dropped, no usable identity", in: map[string]string{opencodeSessionIDHeader: "bad\x01value", legacySessionAffinityHeader: "bad\x02value"}, req: &chatRequest{ModelName: "test-agent", Payload: map[string]any{}}, wantSet: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := newSessionHeaderGW()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.in {
				r.Header.Set(k, v)
			}

			ensureOpencodeSessionHeader(gw, r, tc.req)

			canonical := r.Header.Get(opencodeSessionHeader)
			namespaced := r.Header.Get(opencodeSessionIDHeader)
			if canonical != namespaced {
				t.Fatalf("the two session names must carry one identity: canonical=%q namespaced=%q", canonical, namespaced)
			}
			if !tc.wantSet {
				if canonical != "" || namespaced != "" {
					t.Fatalf("expected no session identity, got %q/%q", canonical, namespaced)
				}
				return
			}
			if tc.wantSynthesized {
				if !strings.HasPrefix(canonical, "nenya-") {
					t.Fatalf("expected a synthesized identity, got %q", canonical)
				}
				if !validSessionHeaderValue(canonical) {
					t.Fatalf("synthesized identity is not a usable header value: %q", canonical)
				}
				return
			}
			if canonical != tc.wantValue {
				t.Fatalf("expected %q, got %q", tc.wantValue, canonical)
			}
		})
	}
}

// TestEnsureSessionParentHeadersForwardedNotSynthesized pins the parent
// relationship handling: usable parent headers ride along, unusable ones
// are dropped, and nothing is invented when the client states none.
func TestEnsureSessionParentHeadersForwardedNotSynthesized(t *testing.T) {
	gw := newSessionHeaderGW()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(opencodeParentSessionHeader, "parent-1")
	ensureOpencodeSessionHeader(gw, r, derivableReq())
	if got := r.Header.Get(opencodeParentSessionHeader); got != "parent-1" {
		t.Fatalf("usable namespaced parent must be preserved, got %q", got)
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(opencodeParentSessionHeader, "bad\x01parent")
	r.Header.Set(legacyParentSessionHeader, "legacy\x02parent")
	ensureOpencodeSessionHeader(gw, r, derivableReq())
	if got := r.Header.Get(opencodeParentSessionHeader); got != "" {
		t.Fatalf("unusable namespaced parent must be dropped, got %q", got)
	}
	if got := r.Header.Get(legacyParentSessionHeader); got != "" {
		t.Fatalf("unusable legacy parent must be dropped, got %q", got)
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ensureOpencodeSessionHeader(gw, r, derivableReq())
	if got := r.Header.Get(opencodeParentSessionHeader); got != "" {
		t.Fatalf("parent must never be synthesized, got %q", got)
	}

	// A client that keeps only the legacy parent name still reaches
	// upstream: the usable value is adopted onto the namespaced name.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(legacyParentSessionHeader, "legacy-parent-1")
	ensureOpencodeSessionHeader(gw, r, derivableReq())
	if got := r.Header.Get(opencodeParentSessionHeader); got != "legacy-parent-1" {
		t.Fatalf("usable legacy parent must be adopted onto the namespaced name, got %q", got)
	}
}

// TestBuildUpstreamRequestForwardsSessionHeaderSet pins the allowlist:
// the namespaced identity and the parent relationship reach the upstream
// request, while unrelated client headers stay stripped.
func TestBuildUpstreamRequestForwardsSessionHeaderSet(t *testing.T) {
	providers := map[string]*config.Provider{
		"test": {
			Name:      "test",
			URL:       "https://api.test.com/v1/chat/completions",
			BaseURL:   "https://api.test.com",
			AuthStyle: "none",
		},
	}
	gw := &gateway.NenyaGateway{
		Config:    config.Config{},
		Secrets:   &config.SecretsConfig{ClientToken: "client-token"},
		Providers: providers,
		Logger:    infra.SetupLogger(false),
	}

	src := http.Header{}
	src.Set(opencodeSessionHeader, "session-xyz")
	src.Set(opencodeSessionIDHeader, "session-xyz")
	src.Set(opencodeParentSessionHeader, "parent-xyz")
	src.Set("X-Custom-Not-Allowed", "leak")

	prx := &Proxy{}
	upstreamReq, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://api.test.com/v1/chat/completions", []byte(`{}`), "test", "", "", src)
	if err != nil {
		t.Fatalf("failed to build upstream request: %v", err)
	}
	if got := upstreamReq.Header.Get(opencodeSessionHeader); got != "session-xyz" {
		t.Errorf("canonical session header not forwarded: %q", got)
	}
	if got := upstreamReq.Header.Get(opencodeSessionIDHeader); got != "session-xyz" {
		t.Errorf("namespaced session header not forwarded: %q", got)
	}
	if got := upstreamReq.Header.Get(opencodeParentSessionHeader); got != "parent-xyz" {
		t.Errorf("parent session header not forwarded: %q", got)
	}
	if got := upstreamReq.Header.Get("X-Custom-Not-Allowed"); got != "" {
		t.Errorf("unrelated header leaked upstream: %q", got)
	}
}
