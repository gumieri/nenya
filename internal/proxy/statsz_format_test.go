package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

const statszTestToken = "nk-statstest-0123456789"

func statszTestProxy(t *testing.T) *Proxy {
	t.Helper()
	gw := gateway.New(t.Context(), *testutil.MinimalConfig(), &config.SecretsConfig{ClientToken: statszTestToken}, testutil.NewTestLogger())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func statszRequest(format string) *http.Request {
	url := "/statsz"
	if format != "" {
		url += "?format=" + format
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.Header.Set("Authorization", "Bearer "+statszTestToken)
	return r
}

// ?format=text renders the same snapshot as deterministic dotted-path lines;
// the JSON form stays the default; an unknown format fails closed with a
// structured 400; and a hostile model name can never forge extra text lines.
func TestStatszFormatNegotiation(t *testing.T) {
	p := statszTestProxy(t)

	// Default (no param): JSON, unchanged.
	w := httptest.NewRecorder()
	p.chainStats(p.Gateway(), w, statszRequest(""))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("the default form must stay JSON: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("the JSON shape must stay parseable: %v", err)
	}

	// Explicit ?format=json is accepted identically to the default.
	w = httptest.NewRecorder()
	p.chainStats(p.Gateway(), w, statszRequest("json"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("format=json must serve the JSON form: %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	// text: flat dotted paths, sorted, with the same content as JSON.
	w = httptest.NewRecorder()
	p.chainStats(p.Gateway(), w, statszRequest("text"))
	if w.Code != http.StatusOK {
		t.Fatalf("format=text must serve: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("format=text must be text/plain: %q", ct)
	}
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("the text form must carry the snapshot: %d lines", len(lines))
	}
	prev := ""
	for _, line := range lines {
		if !strings.Contains(line, ": ") {
			t.Fatalf("every line must be 'path: value': %q", line)
		}
		if line < prev {
			t.Fatalf("lines must be sorted for determinism: %q after %q", line, prev)
		}
		prev = line
	}
	if !strings.Contains(w.Body.String(), "uptime_seconds: ") {
		t.Fatalf("the text form must carry the scalar sections: %s", w.Body.String())
	}

	// Content equivalence: every JSON leaf path must appear as a text line
	// (a divergence — like a section rendered from raw structs — fails here;
	// e.g. the raw latency structs once leaked Go field names and nanosecond
	// TTFT values into the text form).
	jsonBody := map[string]interface{}{}
	jw := httptest.NewRecorder()
	p.chainStats(p.Gateway(), jw, statszRequest(""))
	if err := json.Unmarshal(jw.Body.Bytes(), &jsonBody); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	var expected []string
	collectJSONPaths(jsonBody, nil, &expected)
	if len(expected) == 0 {
		t.Fatal("the equivalence check is vacuous: the snapshot decoded no leaf paths")
	}
	for _, path := range expected {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, path+": ") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the text form lacks a JSON leaf path: %q", path)
		}
	}

	// Unknown format: structured 400, not a silent JSON fallback.
	w = httptest.NewRecorder()
	p.chainStats(p.Gateway(), w, statszRequest("xml"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown format must fail closed: %d", w.Code)
	}
}

// collectJSONPaths gathers the dotted path of every leaf in a decoded JSON
// snapshot (mirroring the text renderer's dotted join).
func collectJSONPaths(node map[string]interface{}, path []string, out *[]string) {
	for k, v := range node {
		child := append(append([]string{}, path...), k)
		if next, ok := v.(map[string]interface{}); ok {
			collectJSONPaths(next, child, out)
			continue
		}
		*out = append(*out, strings.Join(child, "."))
	}
}

// The text renderer cannot be line-forged: a hostile string value renders as
// one JSON-escaped line, never as injected newlines.
func TestRenderStatsTextLineForgery(t *testing.T) {
	var sb strings.Builder
	renderStatsText(&sb, map[string]interface{}{
		"models": map[string]interface{}{
			"evil\": 0\nfake-line: pwned": map[string]interface{}{"requests": 1},
		},
	})
	out := sb.String()
	// The rendered output must be ONE physical line: the hostile key's quote,
	// colon, and newline are JSON-escaped inside the path segment.
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("a hostile key must render as ONE physical line: %q", out)
	}
	if !strings.Contains(out, "fake-line") {
		t.Fatalf("the hostile content must be preserved (escaped): %q", out)
	}

	// A hostile VALUE (not just a key) must also stay on one physical line.
	var vb strings.Builder
	renderStatsText(&vb, map[string]interface{}{
		"note": "line1\nfake-line: pwned",
	})
	vout := vb.String()
	if vlines := strings.Split(strings.TrimRight(vout, "\n"), "\n"); len(vlines) != 1 {
		t.Fatalf("a hostile value must render as ONE physical line: %q", vout)
	}
}

type errConnectionRefused struct{}

func (errConnectionRefused) Error() string { return "connection refused" }

// NENYA-152: a failed MCP discovery surfaces in /statsz as status=failed with
// the last error and timestamp, in BOTH output forms.
func TestStatszShowsFailedMCPServer(t *testing.T) {
	p := statszTestProxy(t)
	gw := p.Gateway()
	gw.SetMCPServerStatus("dead-srv", gateway.MCPServerStatusFailed, errConnectionRefused{})

	// JSON form: the entry carries status/last_error/status_since.
	w := httptest.NewRecorder()
	p.chainStats(gw, w, statszRequest(""))
	var decoded struct {
		MCP map[string]struct {
			Status      string `json:"status"`
			LastError   string `json:"last_error"`
			StatusSince string `json:"status_since"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	entry, ok := decoded.MCP["dead-srv"]
	if !ok {
		t.Fatalf("the dead server must appear in the mcp section: %s", w.Body.String())
	}
	if entry.Status != "failed" || !strings.Contains(entry.LastError, "connection refused") || entry.StatusSince == "" {
		t.Fatalf("the failure must be visible: %+v", entry)
	}

	// Text form: the same fields as dotted paths.
	w = httptest.NewRecorder()
	p.chainStats(gw, w, statszRequest("text"))
	out := w.Body.String()
	for _, want := range []string{
		`mcp.dead-srv.status: "failed"`,
		`mcp.dead-srv.last_error: "connection refused"`,
		`mcp.dead-srv.status_since: "`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the text form must carry %q:\n%s", want, out)
		}
	}
}
