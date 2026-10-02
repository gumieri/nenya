package util

import (
	"math"
	"testing"

	"github.com/nenya/config"
)

func TestAddCap_NoOverflow(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{1, 2, 3},
		{100, 200, 300},
		{math.MaxInt - 1, 1, math.MaxInt - 1 + 1},
		{0, math.MaxInt, math.MaxInt},
		{math.MaxInt, 0, math.MaxInt},
	}
	for _, tt := range tests {
		got := AddCap(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("AddCap(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAddCap_Overflow(t *testing.T) {
	tests := []struct {
		a, b int
	}{
		{math.MaxInt, 1},
		{math.MaxInt, math.MaxInt},
		{math.MaxInt/2 + 1, math.MaxInt/2 + 1},
	}
	for _, tt := range tests {
		got := AddCap(tt.a, tt.b)
		if got != math.MaxInt {
			t.Errorf("AddCap(%d, %d) = %d, want MaxInt on overflow", tt.a, tt.b, got)
		}
	}
}

func TestAddCap_NegativeB(t *testing.T) {
	got := AddCap(10, -5)
	if got != 5 {
		t.Errorf("AddCap(10, -5) = %d, want 5", got)
	}
}

func TestJoinBackticks_Empty(t *testing.T) {
	got := JoinBackticks(nil)
	if got != "" {
		t.Errorf("JoinBackticks(nil) = %q, want empty", got)
	}
	got = JoinBackticks([]string{})
	if got != "" {
		t.Errorf("JoinBackticks([]) = %q, want empty", got)
	}
}

func TestJoinBackticks_Single(t *testing.T) {
	got := JoinBackticks([]string{"foo"})
	if got != "`foo`" {
		t.Errorf("JoinBackticks([foo]) = %q, want `foo`", got)
	}
}

func TestJoinBackticks_Multiple(t *testing.T) {
	got := JoinBackticks([]string{"foo", "bar", "baz"})
	want := "`foo`, `bar`, `baz`"
	if got != want {
		t.Errorf("JoinBackticks = %q, want %q", got, want)
	}
}

func TestProviderCanServe_Nil(t *testing.T) {
	if ProviderCanServe(nil) {
		t.Error("ProviderCanServe(nil) should return false")
	}
}

func TestProviderCanServe_WithAPIKey(t *testing.T) {
	p := &config.Provider{APIKey: "sk-test"}
	if !ProviderCanServe(p) {
		t.Error("ProviderCanServe with API key should return true")
	}
}

func TestProviderCanServe_NoneAuthStyle(t *testing.T) {
	p := &config.Provider{AuthStyle: "none"}
	if !ProviderCanServe(p) {
		t.Error("ProviderCanServe with auth_style 'none' should return true")
	}
}

func TestProviderCanServe_MissingAPIKey(t *testing.T) {
	p := &config.Provider{AuthStyle: "bearer"}
	if ProviderCanServe(p) {
		t.Error("ProviderCanServe without API key and auth_style 'none' should return false")
	}
}

func TestProviderCanServe_BothConditions(t *testing.T) {
	p := &config.Provider{APIKey: "sk-test", AuthStyle: "none"}
	if !ProviderCanServe(p) {
		t.Error("ProviderCanServe with both API key and auth_style 'none' should return true")
	}
}

func TestDeriveInputTokenBudget(t *testing.T) {
	tests := []struct {
		name       string
		maxContext int
		maxOutput  int
		want       int
	}{
		{"unknown output uses three quarters", 1_000_000, 0, 750_000},
		{"output reservation wins", 1_000_000, 384_000, 616_000},
		{"three quarters wins", 1_000_000, 100_000, 750_000},
		{"output equals context falls back", 128_000, 128_000, 96_000},
		{"output exceeds context falls back", 128_000, 200_000, 96_000},
		{"tiny window", 3, 0, 0},
		{"tiny window with output", 10, 8, 2},
		{"tiny window output larger", 4, 3, 1},
		{"zero context disables", 0, 1000, 0},
		{"negative context disables", -5, 1000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveInputTokenBudget(tt.maxContext, tt.maxOutput); got != tt.want {
				t.Errorf("DeriveInputTokenBudget(%d, %d) = %d, want %d", tt.maxContext, tt.maxOutput, got, tt.want)
			}
		})
	}
}

func TestEffectiveOutputTokens(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]interface{}
		declared int
		want     int
	}{
		{"no max_tokens returns declared", map[string]interface{}{}, 384_000, 384_000},
		{"smaller requested wins", map[string]interface{}{"max_tokens": float64(8192)}, 384_000, 8192},
		{"larger requested uses declared", map[string]interface{}{"max_tokens": float64(900_000)}, 384_000, 384_000},
		{"equal requested uses declared", map[string]interface{}{"max_tokens": float64(384_000)}, 384_000, 384_000},
		{"int requested", map[string]interface{}{"max_tokens": 100}, 384_000, 100},
		{"int64 requested", map[string]interface{}{"max_tokens": int64(200)}, 384_000, 200},
		{"negative requested returns declared", map[string]interface{}{"max_tokens": float64(-1)}, 384_000, 384_000},
		{"overflowing float64 returns declared", map[string]interface{}{"max_tokens": float64(math.MaxInt)}, 384_000, 384_000},
		{"NaN returns declared", map[string]interface{}{"max_tokens": math.NaN()}, 384_000, 384_000},
		{"positive infinity returns declared", map[string]interface{}{"max_tokens": math.Inf(1)}, 384_000, 384_000},
		{"non-numeric returns declared", map[string]interface{}{"max_tokens": "big"}, 384_000, 384_000},
		{"requested with unknown declared", map[string]interface{}{"max_tokens": float64(8192)}, 0, 8192},
		{"zero requested with unknown declared", map[string]interface{}{"max_tokens": float64(0)}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveOutputTokens(tt.payload, tt.declared); got != tt.want {
				t.Errorf("EffectiveOutputTokens(%v, %d) = %d, want %d", tt.payload, tt.declared, got, tt.want)
			}
		})
	}
}
