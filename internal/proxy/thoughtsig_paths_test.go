package proxy

import (
	"context"
	"io"
	"log/slog"
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

// TestCacheExtraContentFromResponse pins NENYA-51 gap 1: non-streaming
// responses bypass the SSE transformer, so signatures under
// choices[].message.tool_calls must be extracted into the cache explicitly.
func TestCacheExtraContentFromResponse(t *testing.T) {
	t.Run("message shape", func(t *testing.T) {
		cache := infra.NewThoughtSignatureCache(0, 0)
		resp := map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"message": map[string]interface{}{
						"role": "assistant",
						"tool_calls": []interface{}{
							map[string]interface{}{
								"id":            "call_1",
								"extra_content": map[string]interface{}{"thought_signature": "sig-a"},
							},
							map[string]interface{}{
								"id":            "call_2",
								"extra_content": map[string]interface{}{"thought_signature": "sig-b"},
							},
						},
					},
				},
			},
		}
		cacheExtraContentFromResponse(cache, resp)
		if _, ok := cache.Load("call_1"); !ok {
			t.Error("call_1 signature must be cached")
		}
		if _, ok := cache.Load("call_2"); !ok {
			t.Error("call_2 signature must be cached")
		}
	})
	t.Run("delta shape", func(t *testing.T) {
		cache := infra.NewThoughtSignatureCache(0, 0)
		resp := map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"delta": map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"id":            "call_3",
								"extra_content": "raw-sig",
							},
						},
					},
				},
			},
		}
		cacheExtraContentFromResponse(cache, resp)
		if v, ok := cache.Load("call_3"); !ok || v != "raw-sig" {
			t.Errorf("call_3 signature must be cached, got %v (found=%v)", v, ok)
		}
	})
	t.Run("nil cache and malformed bodies are safe", func(t *testing.T) {
		cacheExtraContentFromResponse(nil, map[string]interface{}{"choices": []interface{}{}})
		cache := infra.NewThoughtSignatureCache(0, 0)
		cacheExtraContentFromResponse(cache, nil)
		cacheExtraContentFromResponse(cache, map[string]interface{}{"choices": "bogus"})
		cacheExtraContentFromResponse(cache, map[string]interface{}{"choices": []interface{}{"bogus"}})
	})
}

// TestCompositeTransformer pins NENYA-51 gap 3: Anthropic-source clients on
// Gemini upstreams must run the provider transformer (signature caching)
// BEFORE the OpenAI→Anthropic converter.
func TestCompositeTransformer(t *testing.T) {
	var order strings.Builder
	provider := &markerTransformer{order: &order, marker: "provider"}
	client := &markerTransformer{order: &order, marker: "client"}

	comp := &compositeTransformer{providerFirst: provider, clientSecond: client}
	out, err := comp.TransformSSEChunk(context.Background(), []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("TransformSSEChunk: %v", err)
	}
	if order.String() != "provider|client" {
		t.Errorf("transformers must run provider-first then client, got %q", order.String())
	}
	if string(out) != "client:provider:{\"x\":1}" {
		t.Errorf("client output must wrap provider output, got %q", string(out))
	}
}

// markerTransformer records invocation order and wraps the payload.
type markerTransformer struct {
	order  *strings.Builder
	marker string
}

func (m *markerTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	if m.order.Len() > 0 {
		m.order.WriteString("|")
	}
	m.order.WriteString(m.marker)
	return []byte(m.marker + ":" + string(data)), nil
}

// TestMCPAccumulatorCarriesExtraContent pins NENYA-51 gap 2: the MCP loop's
// SSE accumulator must carry thought signatures through to the synthetic
// assistant tool_calls, or the sanitizer strips the just-completed tool
// pairs from the loop's own history.
func TestMCPAccumulatorCarriesExtraContent(t *testing.T) {
	acc := newSSEAccumulator(nil)
	lines := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_m","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"extra_content":{"thought_signature":"sig-m"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
	}
	for _, line := range lines {
		if acc.processLine(line) {
			break
		}
	}

	calls := acc.buildToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ExtraContent == nil {
		t.Fatal("extra_content must ride through the accumulator")
	}
	toolCalls := buildOpenAIToolCalls(calls)
	tc, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatal("tool call must be a map")
	}
	if _, has := tc["extra_content"]; !has {
		t.Error("synthetic assistant tool_call must carry extra_content inline")
	}
}

// TestNonStreamThoughtSigRoundTrip pins the NENYA-51 full loop: a
// non-streaming Gemini response with extra_content caches the signature, and
// the NEXT request's history (which omits it) gets it re-injected by the
// sanitizer before dispatch.
func TestNonStreamThoughtSigRoundTrip(t *testing.T) {
	var lastUpstreamBody atomicstring
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastUpstreamBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_n1","type":"function","function":{"name":"get_weather","arguments":"{}"},"extra_content":{"thought_signature":"sig-n1"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"gemini": {URL: up.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"gemini-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "gemini", Model: "gemini-3-flash"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	do := func(history string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gemini-agent","stream":false,"messages":[`+history+`]}`))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// Turn 1: model returns the tool call with its signature.
	rec1 := do(`{"role":"user","content":"weather in SF?"}`)
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1: got %d: %s", rec1.Code, rec1.Body.String())
	}
	if _, ok := gw.ThoughtSigCache.Load("call_n1"); !ok {
		t.Fatal("signature must be cached after the non-stream response")
	}

	// Turn 2: history WITHOUT extra_content — sanitizer must re-inject.
	rec2 := do(`{"role":"user","content":"weather in SF?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_n1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_n1","content":"sunny"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2: got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(lastUpstreamBody.Load(), `"thought_signature"`) {
		t.Errorf("turn 2 upstream body must contain the re-injected signature, got: %s", lastUpstreamBody.Load())
	}
	if strings.Contains(rec2.Body.String(), "missing thought_signature") {
		t.Errorf("tool pair must not be stripped, got: %s", rec2.Body.String())
	}
}

// atomicstring is a tiny string cell for handler-to-test handoff.
type atomicstring struct {
	mu  sync.Mutex
	val string
}

func (a *atomicstring) Store(v string) { a.mu.Lock(); a.val = v; a.mu.Unlock() }
func (a *atomicstring) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.val }
