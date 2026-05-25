package billing

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestQuotaExtractionConfigIsValid(t *testing.T) {
	tests := []struct {
		name    string
		config  QuotaExtractionConfig
		want    bool
	}{
		{
			name: "valid simple_json",
			config: QuotaExtractionConfig{
				Mode:        "simple_json",
				BalancePath: "data.balance",
			},
			want: true,
		},
		{
			name: "valid max_from_array",
			config: QuotaExtractionConfig{
				Mode:         "max_from_array",
				ArrayPath:    "data.accounts",
				ValueField:   "balance",
				ValueDivideBy: 100,
			},
			want: true,
		},
		{
			name: "valid headers",
			config: QuotaExtractionConfig{
				Mode:              "headers",
				RemainingHeader:  "X-RateLimit-Remaining",
				LimitHeader:       "X-RateLimit-Limit",
				ResetHeader:       "X-RateLimit-Reset",
			},
			want: true,
		},
		{
			name: "invalid mode",
			config: QuotaExtractionConfig{
				Mode: "invalid",
			},
			want: false,
		},
		{
			name: "empty mode",
			config: QuotaExtractionConfig{
				Mode: "",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsValid(); got != tt.want {
				t.Errorf("QuotaExtractionConfig.IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetermineExhausted(t *testing.T) {
	tests := []struct {
		name     string
		info     QuotaInfo
		expected bool
	}{
		{
			name: "zero balance",
			info: QuotaInfo{
				BalanceUSD: 0,
			},
			expected: true,
		},
		{
			name: "negative balance",
			info: QuotaInfo{
				BalanceUSD: -1,
			},
			expected: true,
		},
		{
			name: "positive balance",
			info: QuotaInfo{
				BalanceUSD: 10,
			},
			expected: false,
		},
		{
			name: "balance above 1% threshold",
			info: QuotaInfo{
				BalanceUSD: 100,
				LimitUSD:   1000,
			},
			expected: false,
		},
		{
			name: "balance at 1% threshold",
			info: QuotaInfo{
				BalanceUSD: 10,
				LimitUSD:   1000,
			},
			expected: true,
		},
		{
			name: "balance below 1% threshold",
			info: QuotaInfo{
				BalanceUSD: 5,
				LimitUSD:   1000,
			},
			expected: true,
		},
		{
			name: "balance at 0.5% threshold",
			info: QuotaInfo{
				BalanceUSD: 5,
				LimitUSD:   1000,
			},
			expected: true,
		},
		{
			name: "zero limit with positive balance",
			info: QuotaInfo{
				BalanceUSD: 10,
				LimitUSD:   0,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := QuotaInfo{BalanceUSD: tt.info.BalanceUSD, LimitUSD: tt.info.LimitUSD}
			determineExhausted(context.Background(), &result, nil)
			if result.IsExhausted != tt.expected {
				t.Errorf("determineExhausted() = %v, want %v", result.IsExhausted, tt.expected)
			}
		})
	}
}

func TestParseHeaderValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{
			name:  "simple value",
			value: "100",
			want:  100,
		},
		{
			name:  "fractional value",
			value: "99.99",
			want:  99.99,
		},
		{
			name:  "value with total",
			value: "100/1000",
			want:  100,
		},
		{
			name:  "fractional with total",
			value: "99.99/1000.00",
			want:  99.99,
		},
		{
			name:  "whitespace handling",
			value: " 99.99 / 1000.00 ",
			want:  99.99,
		},
		{
			name:    "invalid value",
			value:   "invalid",
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHeaderValue(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseHeaderValue() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseHeaderValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractQuotaFromResponse(t *testing.T) {
	ctx := context.Background()

	t.Run("simple_json valid", func(t *testing.T) {
		body := []byte(`{"data": {"balance": 100.50}}`)
		config := QuotaExtractionConfig{
			Mode:        "simple_json",
			BalancePath: "data.balance",
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() error = %v", err)
		}

		if info.BalanceUSD != 100.50 {
			t.Errorf("BalanceUSD = %v, want 100.50", info.BalanceUSD)
		}
	})

	t.Run("simple_json invalid path", func(t *testing.T) {
		body := []byte(`{"data": {}}`)
		config := QuotaExtractionConfig{
			Mode:        "simple_json",
			BalancePath: "data.balance",
		}

		_, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err == nil {
			t.Error("Expected error for invalid path")
		}
	})

	t.Run("max_from_array valid", func(t *testing.T) {
		body := []byte(`{"accounts": [{"balance": 50}, {"balance": 150}, {"balance": 100}]}`)
		config := QuotaExtractionConfig{
			Mode:         "max_from_array",
			ArrayPath:    "accounts",
			ValueField:   "balance",
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() error = %v", err)
		}

		if info.BalanceUSD != 150 {
			t.Errorf("BalanceUSD = %v, want 150", info.BalanceUSD)
		}
	})

	t.Run("max_from_array with divide", func(t *testing.T) {
		body := []byte(`{"accounts": [{"balance": 5000}, {"balance": 3000}]}`)
		config := QuotaExtractionConfig{
			Mode:         "max_from_array",
			ArrayPath:    "accounts",
			ValueField:   "balance",
			ValueDivideBy: 100,
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() error = %v", err)
		}

		if info.BalanceUSD != 50 {
			t.Errorf("BalanceUSD = %v, want 50", info.BalanceUSD)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		body := []byte(`{}`)
		config := QuotaExtractionConfig{
			Mode: "invalid",
		}

		_, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err == nil {
			t.Error("Expected error for invalid mode")
		}
	})

	t.Run("empty body", func(t *testing.T) {
		body := []byte{}
		config := QuotaExtractionConfig{
			Mode: "simple_json",
		}

		_, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err == nil {
			t.Error("Expected error for empty body")
		}
	})

	t.Run("simple_json with array index path", func(t *testing.T) {
		body := []byte(`{"balance_infos": [{"account": "main", "total_balance": 250.0}]}`)
		config := QuotaExtractionConfig{
			Mode:        "simple_json",
			BalancePath: "balance_infos[0].total_balance",
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() with array index error = %v", err)
		}
		if info.BalanceUSD != 250.0 {
			t.Errorf("BalanceUSD = %v, want 250.0", info.BalanceUSD)
		}
	})

	t.Run("max_from_array with nested path", func(t *testing.T) {
		body := []byte(`{"data": {"limits": [{"group": "standard", "percentage": 0.75}, {"group": "premium", "percentage": 0.95}]}}`)
		config := QuotaExtractionConfig{
			Mode:         "max_from_array",
			ArrayPath:    "data.limits",
			ValueField:   "percentage",
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() error = %v", err)
		}
		if info.BalanceUSD != 0.95 {
			t.Errorf("BalanceUSD = %v, want 0.95", info.BalanceUSD)
		}
	})

	t.Run("max_from_array with limit reset and level", func(t *testing.T) {
		body := []byte(`{"data": {"limits": [{"group": "standard", "percentage": 0.75, "nextReset": 1818000000}, {"group": "premium", "percentage": 0.95, "nextReset": 1819000000}], "level": "premium"}}`)
		config := QuotaExtractionConfig{
			Mode:         "max_from_array",
			ArrayPath:    "data.limits",
			ValueField:   "percentage",
			ResetField:   "data.limits[1].nextReset",
			ResetUnit:    "unix_s",
			LevelField:   "data.level",
		}

		info, err := ExtractQuotaFromResponse(ctx, body, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromResponse() error = %v", err)
		}
		if info.BalanceUSD != 0.95 {
			t.Errorf("BalanceUSD = %v, want 0.95", info.BalanceUSD)
		}
		if info.Level != "premium" {
			t.Errorf("Level = %q, want %q", info.Level, "premium")
		}
		expectedReset := time.Unix(1819000000, 0)
		if !info.ResetAt.Equal(expectedReset) {
			t.Errorf("ResetAt = %v, want %v", info.ResetAt, expectedReset)
		}
	})
}

func TestExtractQuotaFromHeaders(t *testing.T) {
	ctx := context.Background()

	t.Run("headers valid", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("X-RateLimit-Remaining", "5000")
		headers.Set("X-RateLimit-Limit", "10000")
		headers.Set("X-RateLimit-Reset", "1718000000")

		config := QuotaExtractionConfig{
			Mode:              "headers",
			RemainingHeader:  "X-RateLimit-Remaining",
			LimitHeader:       "X-RateLimit-Limit",
			ResetHeader:       "X-RateLimit-Reset",
		}

		info, err := ExtractQuotaFromHeaders(ctx, headers, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromHeaders() error = %v", err)
		}
		if info.BalanceUSD != 5000 {
			t.Errorf("BalanceUSD = %v, want 5000", info.BalanceUSD)
		}
		if info.LimitUSD != 10000 {
			t.Errorf("LimitUSD = %v, want 10000", info.LimitUSD)
		}
		expectedReset := time.Unix(1718000000, 0)
		if !info.ResetAt.Equal(expectedReset) {
			t.Errorf("ResetAt = %v, want %v", info.ResetAt, expectedReset)
		}
	})

	t.Run("headers with slash-separated values", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("X-RateLimit-Remaining", "5000/10000")

		config := QuotaExtractionConfig{
			Mode:             "headers",
			RemainingHeader: "X-RateLimit-Remaining",
		}

		info, err := ExtractQuotaFromHeaders(ctx, headers, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromHeaders() error = %v", err)
		}
		if info.BalanceUSD != 5000 {
			t.Errorf("BalanceUSD = %v, want 5000", info.BalanceUSD)
		}
	})

	t.Run("wrong mode returns error", func(t *testing.T) {
		config := QuotaExtractionConfig{
			Mode: "simple_json",
		}

		_, err := ExtractQuotaFromHeaders(ctx, nil, config, nil)
		if err == nil {
			t.Error("Expected error for wrong mode")
		}
	})

	t.Run("missing headers", func(t *testing.T) {
		config := QuotaExtractionConfig{
			Mode:             "headers",
			RemainingHeader: "X-Nonexistent",
		}

		info, err := ExtractQuotaFromHeaders(ctx, http.Header{}, config, nil)
		if err != nil {
			t.Fatalf("ExtractQuotaFromHeaders() error = %v", err)
		}
		if info.BalanceUSD != 0 {
			t.Errorf("BalanceUSD = %v, want 0 for missing header", info.BalanceUSD)
		}
	})
}