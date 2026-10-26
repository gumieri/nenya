package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/resilience"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// TestForwardBufferedEmpty200FailsOver pins AUD-16 (NENYA-258): the MCP
// buffered sweep must treat an HTTP-200 that carried no SSE data frames as a
// failed upstream attempt — count it in the always-on NENYA-160 degradation
// signal, charge a breaker FAILURE, and fail over to the next target. Pre-fix
// the void recorded a breaker SUCCESS, fired no empty-response counter, and
// was replayed to the client as the answer.
func TestForwardBufferedEmpty200FailsOver(t *testing.T) {
	var emptyHits, goodHits atomic.Int32
	emptyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// 200 with an EMPTY body: the void the loop used to serve.
	}))
	t.Cleanup(emptyUpstream.Close)

	goodUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(goodUpstream.Close)

	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "t"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(gw.Close)
	gw.RateLimiter = infra.NewRateLimiter(0, 0)
	gw.Providers["prov1"] = &config.Provider{Name: "prov1"}
	gw.Providers["prov2"] = &config.Provider{Name: "prov2"}
	// One failure opens the circuit: the void must be charged as a failure.
	gw.AgentState.CB.SetThresholdLookup(func(key string) (failure, success int) { return 1, 1 })

	emptyTarget := routing.UpstreamTarget{
		Provider: "prov1", Model: "empty-model", Format: "openai",
		CoolKey: "agent:prov1:empty-model", URL: emptyUpstream.URL, Credential: "k",
	}
	goodTarget := routing.UpstreamTarget{
		Provider: "prov2", Model: "test-model", Format: "openai",
		CoolKey: "agent:prov2:test-model", URL: goodUpstream.URL, Credential: "k",
	}

	p := &Proxy{}
	p.StoreGateway(gw)

	payload := map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	buf, err := p.forwardBuffered(gw, r.Context(), r, []routing.UpstreamTarget{emptyTarget, goodTarget}, payload, 0, 0, "test-agent", 3, nil, pipeline.CanarResult{}, "openai", false)
	if err != nil {
		t.Fatalf("forwardBuffered: %v", err)
	}
	if buf == nil || !strings.Contains(string(buf.rawBytes), "ok") {
		t.Fatalf("the sweep must fail over to the good target and serve its content, got: %q", bufContent(buf))
	}
	if emptyHits.Load() != 1 || goodHits.Load() != 1 {
		t.Fatalf("upstream hits: empty=%d good=%d, want 1/1 (failover, no retry storm)", emptyHits.Load(), goodHits.Load())
	}

	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_empty_responses_total{kind="buffered", model="empty-model", provider="prov1"} 1`) {
		t.Fatalf("the empty-200 degradation signal must fire for the void target:\n%s", sb.String())
	}
	if state := gw.AgentState.CB.State(emptyTarget.CoolKey); state != resilience.StateOpen {
		t.Fatalf("breaker for the void target = %v, want Open (a failure, not the pre-fix success)", state)
	}
}

func bufContent(buf *bufferedSSE) string {
	if buf == nil {
		return "<nil>"
	}
	return string(buf.rawBytes)
}
