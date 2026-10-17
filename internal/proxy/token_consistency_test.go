package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"strconv"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
	"github.com/nenya/internal/tiktoken"
)

// TestEstimateAgreesWithUpstreamUsage pins NENYA-157's consistency requirement:
// the pre-dispatch estimate and the upstream's reported prompt usage describe
// the same request within a tolerance, and the calibration loop records the
// pair so its EMA applies on top of the estimate (the estimate is the seed, the
// usage payload is the observed truth).
func TestEstimateAgreesWithUpstreamUsage(t *testing.T) {
	// A long, ordinary text turn: the estimate should track the usage closely.
	text := strings.Repeat("Please summarize the following function and list its edge cases. ", 12)

	var reported int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The upstream "usage" is the real cl100k count of the dispatched
		// body, the closest stand-in for a provider's prompt accounting.
		count, err := tiktoken.CountTokens(string(body))
		if err != nil {
			t.Errorf("tokenize: %v", err)
		}
		atomic.StoreInt64(&reported, int64(count))
		usageJSON := strconv.Itoa(count)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
			usageJSON + `,"completion_tokens":1,"total_tokens":` + usageJSON + `}}`))
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Providers = map[string]config.ProviderConfig{
		"usage-provider": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "usage-provider", Model: "test-model"}},
		},
	}
	gw := gateway.New(t.Context(), *cfg, &config.SecretsConfig{ClientToken: "test-token"}, testLog(t))
	p := &Proxy{}
	p.StoreGateway(gw)

	payload := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"` + text + `"}]}`
	rec := postChatJSON(t, p, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The estimate the request carried is the same composed measure the
	// budget seed uses, and it is what the calibration loop paired with the
	// upstream usage.
	estimate := gw.CountRequestTokens(map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": text}},
	})
	usage := int(atomic.LoadInt64(&reported))
	if usage == 0 {
		t.Fatal("the upstream never reported usage")
	}
	// The usage counts the whole serialized body (JSON syntax and framing
	// included), so it is expected to exceed the composed estimate; the
	// tolerance is the composition's accuracy, not equality.
	ratio := float64(estimate) / float64(usage)
	if ratio < 0.6 || ratio > 1.1 {
		t.Fatalf("estimate %d vs upstream usage %d (ratio %.2f) is outside the tolerance", estimate, usage, ratio)
	}

	stats := gw.Calibration.Snapshot()
	stat, ok := stats["test-model"]
	if !ok || stat.Records == 0 {
		t.Fatalf("the calibration loop recorded no observation for the request: %+v", stats)
	}
	// The recorded pair is (composed estimate, upstream usage): the ratio must
	// be the one those two numbers imply, within the warm-up smoothing.
	if stat.EstimateTokens <= 0 || stat.ActualTokens <= 0 {
		t.Fatalf("calibration recorded a zero side of the pair: %+v", stat)
	}
	wantRatio := stat.ActualTokens / stat.EstimateTokens
	if diff := stat.Ratio - wantRatio; diff > 0.5 || diff < -0.5 {
		t.Fatalf("ratio %v does not track the recorded pair (%v / %v = %v)", stat.Ratio, stat.ActualTokens, stat.EstimateTokens, wantRatio)
	}
	if stat.EstimateTokens < float64(estimate)*0.9 || stat.EstimateTokens > float64(estimate)*1.1 {
		t.Fatalf("the recorded estimate %v does not match the composed estimate %d the request carried", stat.EstimateTokens, estimate)
	}
}
