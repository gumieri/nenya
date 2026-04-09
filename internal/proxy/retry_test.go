package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/routing"
)

func TestParseRetryDelay_NoHeaderNoBody(t *testing.T) {
	d := parseRetryDelay(http.Header{}, nil)
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelay_RetryAfterHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	d := parseRetryDelay(h, nil)
	if d != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d)
	}
}

func TestParseRetryDelay_RetryAfterHeaderCapped(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "999")
	d := parseRetryDelay(h, nil)
	if d != maxRetryBackoff {
		t.Fatalf("expected %v, got %v", maxRetryBackoff, d)
	}
}

func TestParseRetryDelay_GeminiRPCRetryDelay(t *testing.T) {
	body := `{
		"error": {
			"details": [{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]
		}
	}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d)
	}
}

func TestParseRetryDelay_GeminiRPCRetryDelayCapped(t *testing.T) {
	body := `{
		"error": {
			"details": [{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]
		}
	}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != maxRetryBackoff {
		t.Fatalf("expected %v, got %v", maxRetryBackoff, d)
	}
}

func TestParseRetryDelay_InvalidBody(t *testing.T) {
	d := parseRetryDelay(http.Header{}, []byte("not json at all"))
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelay_EmptyBody(t *testing.T) {
	d := parseRetryDelay(http.Header{}, []byte{})
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelay_HeaderTakesPriorityOverBody(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "2")
	body := `{
		"error": {
			"details": [{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"4s"}]
		}
	}`
	d := parseRetryDelay(h, []byte(body))
	if d != 2*time.Second {
		t.Fatalf("expected 2s (from header), got %v", d)
	}
}

func TestParseRetryDelay_OpenAIStyleWait(t *testing.T) {
	body := `{"error":{"message":"Rate limit reached. Please wait 3 seconds before retrying."}}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d)
	}
}

func TestParseRetryDelay_OpenAIStyleRetryIn(t *testing.T) {
	body := `{"error":{"message":"Please retry in 5 seconds."}}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 5*time.Second {
		t.Fatalf("expected 5s, got %v", d)
	}
}

func TestParseRetryDelay_OpenAIStyleCapped(t *testing.T) {
	body := `{"error":{"message":"Please wait 999 seconds before retrying."}}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != maxRetryBackoff {
		t.Fatalf("expected %v, got %v", maxRetryBackoff, d)
	}
}

func TestParseQuotaExhaustion_Per86400s(t *testing.T) {
	body := []byte(`{"error":"quota exceeded: per 86400s"}`)
	d := parseQuotaExhaustion(body)
	if d != maxQuotaCooldown {
		t.Fatalf("expected %v, got %v", maxQuotaCooldown, d)
	}
}

func TestParseQuotaExhaustion_Perday(t *testing.T) {
	body := []byte(`{"error":"perday limit reached"}`)
	d := parseQuotaExhaustion(body)
	if d != maxQuotaCooldown {
		t.Fatalf("expected %v, got %v", maxQuotaCooldown, d)
	}
}

func TestParseQuotaExhaustion_ResourceExhausted(t *testing.T) {
	body := []byte(`{"error":{"code":"RESOURCE_EXHAUSTED"}}`)
	d := parseQuotaExhaustion(body)
	if d != 5*time.Minute {
		t.Fatalf("expected 5m, got %v", d)
	}
}

func TestParseQuotaExhaustion_QuotaExceededSpace(t *testing.T) {
	body := []byte(`{"message":"Quota Exceeded for this account"}`)
	d := parseQuotaExhaustion(body)
	if d != 5*time.Minute {
		t.Fatalf("expected 5m, got %v", d)
	}
}

func TestParseQuotaExhaustion_QuotaExceededUnderscore(t *testing.T) {
	body := []byte(`{"error":"quota_exceeded"}`)
	d := parseQuotaExhaustion(body)
	if d != 5*time.Minute {
		t.Fatalf("expected 5m, got %v", d)
	}
}

func TestParseQuotaExhaustion_NoMatch(t *testing.T) {
	body := []byte(`{"error":"internal server error"}`)
	d := parseQuotaExhaustion(body)
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseQuotaExhaustion_EmptyBody(t *testing.T) {
	d := parseQuotaExhaustion([]byte{})
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestIsRetryableClientError_UnavailableModel(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"unavailable_model"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_TokensLimitReached(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"tokens_limit_reached"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_ContextLengthExceeded(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"context_length_exceeded"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_ThoughtSignature(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"thought_signature"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_UnknownPattern(t *testing.T) {
	if isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"something_else"}`)) {
		t.Fatal("expected false")
	}
}

func TestIsRetryableClientError_413WithContextLength(t *testing.T) {
	if !isRetryableClientError(http.StatusRequestEntityTooLarge, []byte(`{"error":"context_length_exceeded"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_500NotRetryable(t *testing.T) {
	if isRetryableClientError(http.StatusInternalServerError, []byte(`{"error":"context_length_exceeded"}`)) {
		t.Fatal("expected false")
	}
}

func TestIsRetryableClientError_EmptyBody(t *testing.T) {
	if isRetryableClientError(http.StatusBadRequest, []byte{}) {
		t.Fatal("expected false")
	}
}

func TestIsRetryableStatus_DefaultCodes(t *testing.T) {
	p := &Proxy{GW: newTestGateway(nil, nil)}
	for _, code := range defaultRetryableStatusCodes {
		if !p.isRetryableStatus("unknown_provider", code) {
			t.Fatalf("expected %d to be retryable", code)
		}
	}
}

func TestIsRetryableStatus_NonRetryableCodes(t *testing.T) {
	p := &Proxy{GW: newTestGateway(nil, nil)}
	nonRetryable := []int{400, 401, 403, 404}
	for _, code := range nonRetryable {
		if p.isRetryableStatus("unknown_provider", code) {
			t.Fatalf("expected %d to NOT be retryable", code)
		}
	}
}

func TestIsRetryableStatus_CustomProviderCodes(t *testing.T) {
	providers := map[string]*config.Provider{
		"custom": {
			Name:                 "custom",
			RetryableStatusCodes: []int{401, 403},
		},
	}
	p := &Proxy{GW: newTestGateway(nil, providers)}
	if !p.isRetryableStatus("custom", 401) {
		t.Fatal("expected 401 to be retryable for custom provider")
	}
	if p.isRetryableStatus("custom", 429) {
		t.Fatal("expected 429 to NOT be retryable for custom provider (custom list overrides)")
	}
}

func TestIsRetryableStatus_GlobalConfigCodes(t *testing.T) {
	cfg := config.Config{}
	cfg.Governance.RetryableStatusCodes = []int{502, 504}
	p := &Proxy{GW: newTestGateway(&cfg, nil)}
	if !p.isRetryableStatus("unknown_provider", 502) {
		t.Fatal("expected 502 to be retryable via global config")
	}
	if p.isRetryableStatus("unknown_provider", 429) {
		t.Fatal("expected 429 to NOT be retryable (global config overrides defaults)")
	}
}

func TestCapRetryDelay_Zero(t *testing.T) {
	if capRetryDelay(0) != 0 {
		t.Fatal("expected 0")
	}
}

func TestCapRetryDelay_Negative(t *testing.T) {
	if capRetryDelay(-5 * time.Second) != 0 {
		t.Fatal("expected 0")
	}
}

func TestCapRetryDelay_WithinCap(t *testing.T) {
	if capRetryDelay(3 * time.Second) != 3*time.Second {
		t.Fatal("expected 3s")
	}
}

func TestCapRetryDelay_OverCap(t *testing.T) {
	if capRetryDelay(60 * time.Second) != maxRetryBackoff {
		t.Fatal("expected maxRetryBackoff")
	}
}

func TestParseRetryDelayFromRPCDetails_ValidDelay(t *testing.T) {
	details := []rpcDetail{
		{RetryDelay: "2s", Type: "type.googleapis.com/google.rpc.RetryInfo"},
	}
	d := parseRetryDelayFromRPCDetails(details)
	if d != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d)
	}
}

func TestParseRetryDelayFromRPCDetails_EmptyDetails(t *testing.T) {
	d := parseRetryDelayFromRPCDetails(nil)
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelayFromRPCDetails_InvalidDuration(t *testing.T) {
	details := []rpcDetail{
		{RetryDelay: "not-a-duration"},
	}
	d := parseRetryDelayFromRPCDetails(details)
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_RetryIn(t *testing.T) {
	d := parseRetryDelayFromMessage("Please retry in 3 seconds")
	if d != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_Wait(t *testing.T) {
	d := parseRetryDelayFromMessage("Please wait 2 seconds before retrying")
	if d != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_RetryAfter(t *testing.T) {
	d := parseRetryDelayFromMessage("retry after 5 seconds")
	if d != 5*time.Second {
		t.Fatalf("expected 5s, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_NoNumber(t *testing.T) {
	d := parseRetryDelayFromMessage("retry in seconds")
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_NoMatch(t *testing.T) {
	d := parseRetryDelayFromMessage("something completely unrelated")
	if d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryDelayFromMessage_Capped(t *testing.T) {
	d := parseRetryDelayFromMessage("wait 999 seconds")
	if d != maxRetryBackoff {
		t.Fatalf("expected %v, got %v", maxRetryBackoff, d)
	}
}

func TestParseRetryDelay_ArrayBody(t *testing.T) {
	body := `[{"error":{"message":"retry in 2 seconds"}}]`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d)
	}
}

func TestParseRetryDelay_RPCDetailInArrayBody(t *testing.T) {
	body := `[{"error":{"details":[{"retryDelay":"4s"}]}}]`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 4*time.Second {
		t.Fatalf("expected 4s, got %v", d)
	}
}

func newTestGateway(cfg *config.Config, providers map[string]*config.Provider) *gateway.NenyaGateway {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if providers == nil {
		providers = make(map[string]*config.Provider)
	}
	return &gateway.NenyaGateway{
		Config:     *cfg,
		Logger:     slog.Default(),
		Stats:      infra.NewUsageTracker(),
		Metrics:    infra.NewMetrics(),
		AgentState: routing.NewAgentState(),
		Providers:  providers,
	}
}

func TestParseRetryDelay_RetryAfterHeaderNonNumeric(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "abc")
	d := parseRetryDelay(h, nil)
	if d != 0 {
		t.Fatalf("expected 0 for non-numeric header, got %v", d)
	}
}

func TestParseRetryDelay_BodyRPCDetailBeforeMessage(t *testing.T) {
	body := `{
		"error": {
			"details": [{"retryDelay":"1s"}],
			"message": "wait 99 seconds"
		}
	}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 1*time.Second {
		t.Fatalf("expected 1s (RPC detail takes priority), got %v", d)
	}
}

func TestIsRetryableClientError_ModelOverloaded(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"model_overloaded"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_Overloaded(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"overloaded"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_NameCannotBeEmpty(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"name cannot be empty"}`)) {
		t.Fatal("expected true")
	}
}

func TestIsRetryableClientError_MessagesParameterIllegal(t *testing.T) {
	if !isRetryableClientError(http.StatusBadRequest, []byte(`{"error":"messages parameter is illegal"}`)) {
		t.Fatal("expected true")
	}
}

func TestParseRetryDelay_FloatSeconds(t *testing.T) {
	body := `{"error":{"message":"Please wait 1.5 seconds"}}`
	d := parseRetryDelay(http.Header{}, []byte(body))
	if d != 1*time.Second {
		t.Fatalf("expected 1s, got %v", d)
	}
}

func TestParseRetryDelay_RetryAfterZero(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "0")
	d := parseRetryDelay(h, nil)
	if d != 0 {
		t.Fatalf("expected 0 for zero value, got %v", d)
	}
}

func TestParseRetryDelay_NegativeSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "-1")
	d := parseRetryDelay(h, nil)
	if d != 0 {
		t.Fatalf("expected 0 for negative value, got %v", d)
	}
}

func TestIsRetryableStatus_ProviderOverridesGlobal(t *testing.T) {
	cfg := config.Config{}
	cfg.Governance.RetryableStatusCodes = []int{502, 504}
	providers := map[string]*config.Provider{
		"myprovider": {
			Name:                 "myprovider",
			RetryableStatusCodes: []int{401},
		},
	}
	p := &Proxy{GW: newTestGateway(&cfg, providers)}
	if !p.isRetryableStatus("myprovider", 401) {
		t.Fatal("expected provider-specific code to take priority")
	}
	if p.isRetryableStatus("myprovider", 502) {
		t.Fatal("expected global config code to NOT apply when provider overrides")
	}
}

func BenchmarkParseRetryDelay_HeaderOnly(b *testing.B) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	for i := 0; i < b.N; i++ {
		parseRetryDelay(h, nil)
	}
}

func BenchmarkParseRetryDelay_RPCBody(b *testing.B) {
	body := []byte(fmt.Sprintf(`{
		"error": {
			"details": [{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]
		}
	}`))
	for i := 0; i < b.N; i++ {
		parseRetryDelay(http.Header{}, body)
	}
}

func BenchmarkIsRetryableClientError(b *testing.B) {
	body := []byte(`{"error":"unavailable_model"}`)
	for i := 0; i < b.N; i++ {
		isRetryableClientError(http.StatusBadRequest, body)
	}
}

func BenchmarkParseQuotaExhaustion(b *testing.B) {
	body := []byte(`{"error":"quota exceeded: per 86400s"}`)
	for i := 0; i < b.N; i++ {
		parseQuotaExhaustion(body)
	}
}

func FuzzParseRetryDelay(f *testing.F) {
	f.Fuzz(func(t *testing.T, headerVal string, body []byte) {
		h := http.Header{}
		if headerVal != "" {
			h.Set("Retry-After", headerVal)
		}
		d := parseRetryDelay(h, body)
		if d < 0 {
			t.Errorf("negative duration: %v", d)
		}
		if d > maxRetryBackoff {
			t.Errorf("exceeds cap: %v > %v", d, maxRetryBackoff)
		}
	})
}

func FuzzParseQuotaExhaustion(f *testing.F) {
	f.Fuzz(func(t *testing.T, body []byte) {
		d := parseQuotaExhaustion(body)
		if d < 0 {
			t.Errorf("negative duration: %v", d)
		}
		if d > maxQuotaCooldown {
			t.Errorf("exceeds cap: %v > %v", d, maxQuotaCooldown)
		}
	})
}

func FuzzIsRetryableClientError(f *testing.F) {
	f.Fuzz(func(t *testing.T, statusCode int, body []byte) {
		isRetryableClientError(statusCode, body)
	})
}

func FuzzParseRetryDelayFromMessage(f *testing.F) {
	f.Fuzz(func(t *testing.T, msg string) {
		d := parseRetryDelayFromMessage(msg)
		if d < 0 {
			t.Errorf("negative duration: %v", d)
		}
		if d > maxRetryBackoff {
			t.Errorf("exceeds cap: %v > %v", d, maxRetryBackoff)
		}
	})
}
