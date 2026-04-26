package adapter

import "testing"

func TestZAIAdapter_NormalizeError(t *testing.T) {
	a := NewZAIAdapter(ZAIAdapterDeps{})

	tests := []struct {
		name string
		code int
		body string
		want ErrorClass
	}{
		{"concurrency_1302", 429, `{"error":{"code":"1302"}}`, ErrorRateLimited},
		{"frequency_1303", 429, `{"error":{"code":"1303"}}`, ErrorRateLimited},
		{"usage_limit_1308", 429, `{"error":{"code":"1308"}}`, ErrorQuotaExhausted},
		{"weekly_limit_1310", 429, `{"error":{"code":"1310"}}`, ErrorQuotaExhausted},
		{"high_traffic_1312", 429, `{"error":{"code":"1312"}}`, ErrorRetryable},
		{"no_subscription_1311", 429, `{"error":{"code":"1311"}}`, ErrorPermanent},
		{"fair_use_1313", 429, `{"error":{"code":"1313"}}`, ErrorPermanent},
		{"unknown_code_429", 429, `{"error":{"code":"9999"}}`, ErrorRateLimited},
		{"generic_429", 429, `{}`, ErrorRateLimited},
		{"generic_500", 500, `{}`, ErrorRetryable},
		{"empty_body_429", 429, ``, ErrorRateLimited},
		{"malformed_json_429", 429, `{invalid`, ErrorRateLimited},
		{"quota_on_403", 403, `{"error":{"code":"1310"}}`, ErrorQuotaExhausted},
		{"concurrency_on_500", 500, `{"error":{"code":"1302"}}`, ErrorRateLimited},
		{"generic_400", 400, `{"error":{"code":"1311"}}`, ErrorPermanent},
		{"context_window_exceeded", 400, `{"error":{"message":"model_context_window_exceeded"}}`, ErrorRetryable},
		{"context_window_exceeded_in_message", 400, `{"error":{"message":"request failed: model_context_window_exceeded for model glm-5"}}`, ErrorRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.NormalizeError(tt.code, []byte(tt.body))
			if got != tt.want {
				t.Errorf("NormalizeError(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
			}
		})
	}
}
