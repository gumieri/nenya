package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// filtersGW builds a gateway with two Anthropic-format providers carrying
// different header filters, plus one unfiltered provider, and captures the
// metrics exposition.
func filtersGW() *gateway.NenyaGateway {
	return &gateway.NenyaGateway{
		Config: config.Config{},
		Providers: map[string]*config.Provider{
			"agg-deny": {
				Name: "agg-deny", URL: "https://agg.test/v1/messages", BaseURL: "https://agg.test",
				ApiFormat: "anthropic", AuthStyle: "none",
				HeaderFilters: map[string]config.HeaderFilter{
					"Anthropic-Beta": {Deny: []string{"context-management"}},
				},
			},
			"agg-allow": {
				Name: "agg-allow", URL: "https://allow.test/v1/messages", BaseURL: "https://allow.test",
				ApiFormat: "anthropic", AuthStyle: "none",
				HeaderFilters: map[string]config.HeaderFilter{
					"anthropic-beta": {Allow: []string{"EXTENDED-CACHE-TTL"}},
				},
			},
			"plain": {
				Name: "plain", URL: "https://plain.test/v1/messages", BaseURL: "https://plain.test",
				ApiFormat: "anthropic", AuthStyle: "none",
			},
		},
		Secrets: &config.SecretsConfig{ClientToken: "client-token"},
		Logger:  infra.SetupLogger(false),
		Metrics: infra.NewMetrics(),
	}
}

// mixedBeta is the client's versioned beta header: one value an aggregator
// rejects, one it accepts.
const mixedBeta = "extended-cache-ttl-2025-04-11, context-management-2025-06-27"

// TestHeaderFiltersStrippedPerTarget pins NENYA-201: the same mixed-value
// header is filtered per provider — a deny strips the rejected version for
// that backend while an allow on another backend keeps only its match, and
// an unfiltered provider receives the value verbatim.
func TestHeaderFiltersStrippedPerTarget(t *testing.T) {
	gw := filtersGW()
	src := http.Header{}
	src.Set("Anthropic-Beta", mixedBeta)
	prx := &Proxy{}

	build := func(provider, url string) *http.Request {
		t.Helper()
		req, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
			url, []byte(`{}`), provider, "", "", src)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		return req
	}

	if got := build("agg-deny", "https://agg.test/v1/messages").Header.Get("Anthropic-Beta"); got != "extended-cache-ttl-2025-04-11" {
		t.Fatalf("deny filter must strip only the rejected version, got %q", got)
	}
	if got := build("agg-allow", "https://allow.test/v1/messages").Header.Get("Anthropic-Beta"); got != "extended-cache-ttl-2025-04-11" {
		t.Fatalf("allow filter must keep only the matching version, got %q", got)
	}
	if got := build("plain", "https://plain.test/v1/messages").Header.Get("Anthropic-Beta"); got != mixedBeta {
		t.Fatalf("an unfiltered provider must receive the header verbatim, got %q", got)
	}
}

// TestHeaderFiltersDropAndRepeat pins the edge shapes: every value of a
// header stripped means the header is removed; repeated header lines are
// filtered individually; filters are inert for other headers.
func TestHeaderFiltersDropAndRepeat(t *testing.T) {
	gw := filtersGW()
	prx := &Proxy{}

	// Deny-everything: the header disappears entirely.
	src := http.Header{}
	src.Set("Anthropic-Beta", "context-management-2025-06-27")
	req, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://agg.test/v1/messages", []byte(`{}`), "agg-deny", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("a header with no surviving values must be removed, got %q", got)
	}

	// Repeated header lines are filtered per value.
	src = http.Header{}
	src.Add("Anthropic-Beta", "extended-cache-ttl-2025-04-11")
	src.Add("Anthropic-Beta", "context-management-2025-06-27")
	req, err = prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://agg.test/v1/messages", []byte(`{}`), "agg-deny", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Values("Anthropic-Beta"); len(got) != 1 || !strings.Contains(got[0], "extended-cache-ttl") {
		t.Fatalf("repeated lines must be filtered per value, got %q", got)
	}

	// Other headers are untouched; a filter on a header the request does
	// not carry is inert.
	src = http.Header{}
	src.Set("X-Request-Id", "req-1")
	req, err = prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://agg.test/v1/messages", []byte(`{}`), "agg-deny", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("unrelated headers must pass through, got %q", got)
	}
}

// TestHeaderFiltersCoverGatewayInjectedHeaders pins that the filter pass
// runs on the COMPLETE outbound set (NENYA-201 review): the session-identity
// headers the gateway injects itself are filterable per target, not only
// client passthrough.
func TestHeaderFiltersCoverGatewayInjectedHeaders(t *testing.T) {
	gw := filtersGW()
	gw.Providers["agg-deny"].HeaderFilters["X-Opencode-Session"] = config.HeaderFilter{
		Deny: []string{"partner-"},
	}
	src := http.Header{}
	src.Set("X-Opencode-Session", "partner-abc-123")
	prx := &Proxy{}
	req, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://agg.test/v1/messages", []byte(`{}`), "agg-deny", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Opencode-Session"); got != "" {
		t.Fatalf("a gateway-injected header must be subject to the target's filter, got %q", got)
	}
}

// TestHeaderFiltersCounted pins the telemetry: every stripped value
// increments nenya_header_values_filtered_total{provider,header}, rendered
// in the exposition.
func TestHeaderFiltersCounted(t *testing.T) {
	gw := filtersGW()
	src := http.Header{}
	src.Set("Anthropic-Beta", mixedBeta)
	prx := &Proxy{}
	if _, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://agg.test/v1/messages", []byte(`{}`), "agg-deny", "", "", src); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	gw.Metrics.WritePrometheus(&buf)
	want := `nenya_header_values_filtered_total{header="Anthropic-Beta", provider="agg-deny"} 1`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected %q in the exposition, got:\n%s", want, buf.String())
	}
}

// TestHeaderValueMatchesSemantics pins the filter algebra: allow is an
// allowlist, deny removes after allow, both empty is inert, and matches
// are case-insensitive substrings.
func TestHeaderValueMatchesSemantics(t *testing.T) {
	cases := []struct {
		value  string
		filter config.HeaderFilter
		want   bool
	}{
		{"context-management-2025-06-27", config.HeaderFilter{Deny: []string{"context-management"}}, false},
		{"extended-cache-ttl-2025-04-11", config.HeaderFilter{Deny: []string{"context-management"}}, true},
		{"extended-cache-ttl-2025-04-11", config.HeaderFilter{Allow: []string{"extended-cache"}}, true},
		{"context-management-2025-06-27", config.HeaderFilter{Allow: []string{"extended-cache"}}, false},
		{"extended-cache-ttl-2025-04-11", config.HeaderFilter{Allow: []string{"EXTENDED-CACHE"}, Deny: []string{"context"}}, true},
		{"interleaved-thinking-2025-05-14", config.HeaderFilter{Allow: []string{"extended-cache"}, Deny: []string{"thinking"}}, false},
		// A value matching BOTH lists is denied: deny wins over allow.
		{"extended-cache-ttl-2025-04-11", config.HeaderFilter{Allow: []string{"cache"}, Deny: []string{"extended"}}, false},
		{"anything", config.HeaderFilter{}, true},
	}
	for _, tc := range cases {
		if got := headerValueMatches(tc.value, lowerFilter(tc.filter)); got != tc.want {
			t.Errorf("headerValueMatches(%q, %+v) = %v, want %v", tc.value, tc.filter, got, tc.want)
		}
	}
}
