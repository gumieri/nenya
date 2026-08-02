package config

import (
	"testing"
	"time"
)

func TestApplyBuiltInProviders_MergesPartialConfig(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {
				URL: "http://192.168.0.9:11434/v1/chat/completions",
			},
		},
	}

	applyBuiltInProviders(cfg)

	ollama, ok := cfg.Providers["ollama"]
	if !ok {
		t.Fatal("ollama provider not found after applyBuiltInProviders")
	}

	if ollama.URL != "http://192.168.0.9:11434/v1/chat/completions" {
		t.Errorf("URL = %v, want http://192.168.0.9:11434/v1/chat/completions", ollama.URL)
	}

	if ollama.AuthStyle != "none" {
		t.Errorf("AuthStyle = %v, want none", ollama.AuthStyle)
	}

	if ollama.ApiFormat != "" {
		t.Errorf("ApiFormat should be empty, got %v", ollama.ApiFormat)
	}
}

func TestApplyBuiltInProviders_UserOverridesDefaults(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"anthropic": {
				URL:       "https://custom.anthropic.com/v1/messages",
				AuthStyle: "bearer",
			},
		},
	}

	applyBuiltInProviders(cfg)

	anthropic, ok := cfg.Providers["anthropic"]
	if !ok {
		t.Fatal("anthropic provider not found after applyBuiltInProviders")
	}

	if anthropic.URL != "https://custom.anthropic.com/v1/messages" {
		t.Errorf("URL = %v, want user-provided custom URL", anthropic.URL)
	}

	if anthropic.AuthStyle != "bearer" {
		t.Errorf("AuthStyle = %v, want user-provided bearer", anthropic.AuthStyle)
	}
}

func TestApplyBuiltInProviders_AddsMissingProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{},
	}

	applyBuiltInProviders(cfg)

	if _, ok := cfg.Providers["anthropic"]; !ok {
		t.Error("anthropic provider not added")
	}

	if _, ok := cfg.Providers["openai"]; !ok {
		t.Error("openai provider not added")
	}

	if _, ok := cfg.Providers["ollama"]; !ok {
		t.Error("ollama provider not added")
	}
}

func TestMergeProviderConfig_RespectsUserValues(t *testing.T) {
	user := ProviderConfig{
		URL:            "http://custom.com",
		AuthStyle:      "bearer",
		ApiFormat:      "openai",
		TimeoutSeconds: 30,
	}

	builtIn := ProviderConfig{
		URL:            "http://builtin.com",
		AuthStyle:      "none",
		ApiFormat:      "anthropic",
		TimeoutSeconds: 60,
	}

	merged := mergeProviderConfig(user, builtIn)

	if merged.URL != "http://custom.com" {
		t.Errorf("URL = %v, want user value", merged.URL)
	}

	if merged.AuthStyle != "bearer" {
		t.Errorf("AuthStyle = %v, want user value", merged.AuthStyle)
	}

	if merged.ApiFormat != "openai" {
		t.Errorf("ApiFormat = %v, want user value", merged.ApiFormat)
	}

	if merged.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %v, want user value", merged.TimeoutSeconds)
	}
}

func TestMergeProviderConfig_FillsInMissingDefaults(t *testing.T) {
	user := ProviderConfig{
		URL: "http://custom.com",
	}

	builtIn := ProviderConfig{
		URL:                  "http://builtin.com",
		AuthStyle:            "none",
		ApiFormat:            "openai",
		TimeoutSeconds:       60,
		MaxRetryAttempts:     3,
		RetryableStatusCodes: []int{429, 500, 502, 503},
		FormatURLs: map[string]string{
			"anthropic": "http://builtin.com/v1/messages",
		},
	}

	merged := mergeProviderConfig(user, builtIn)

	if merged.URL != "http://custom.com" {
		t.Errorf("URL = %v, want user value", merged.URL)
	}

	if merged.AuthStyle != "none" {
		t.Errorf("AuthStyle = %v, want built-in value", merged.AuthStyle)
	}

	if merged.ApiFormat != "openai" {
		t.Errorf("ApiFormat = %v, want built-in value", merged.ApiFormat)
	}

	if merged.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %v, want built-in value", merged.TimeoutSeconds)
	}

	if merged.MaxRetryAttempts != 3 {
		t.Errorf("MaxRetryAttempts = %v, want built-in value", merged.MaxRetryAttempts)
	}

	if len(merged.RetryableStatusCodes) != 4 {
		t.Errorf("RetryableStatusCodes = %v, want built-in value", merged.RetryableStatusCodes)
	}

	if len(merged.FormatURLs) != 1 {
		t.Errorf("FormatURLs = %v, want built-in value", merged.FormatURLs)
	}

	if merged.FormatURLs["anthropic"] != "http://builtin.com/v1/messages" {
		t.Errorf("FormatURLs[anthropic] = %v, want built-in value", merged.FormatURLs["anthropic"])
	}
}

func TestMergeString_NilDst(t *testing.T) {
	mergeString(nil, "value")
}

func TestMergeString_EmptyDst(t *testing.T) {
	dst := ""
	mergeString(&dst, "value")
	if dst != "value" {
		t.Errorf("dst = %v, want value", dst)
	}
}

func TestMergeString_NonEmptyDst(t *testing.T) {
	dst := "user"
	mergeString(&dst, "value")
	if dst != "user" {
		t.Errorf("dst = %v, want user", dst)
	}
}

func TestMergeString_EmptySrc(t *testing.T) {
	dst := ""
	mergeString(&dst, "")
	if dst != "" {
		t.Errorf("dst = %v, want empty", dst)
	}
}

func TestPrefixCache_ValidTTL_AcceptsEphemeral(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         true,
			CacheControlTTL: "ephemeral",
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Errorf("expected no error for valid TTL 'ephemeral', got: %v", err)
	}
}

func TestPrefixCache_ValidTTL_Accepts1h(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         true,
			CacheControlTTL: "1h",
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Errorf("expected no error for valid TTL '1h', got: %v", err)
	}
}

func TestPrefixCache_InvalidTTL_Rejected(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         true,
			CacheControlTTL: "bogus",
		},
	}
	err := ApplyDefaults(cfg)
	if err == nil {
		t.Fatal("expected error for invalid TTL 'bogus', got nil")
	}
	expectedMsg := `invalid cache_control_ttl: "bogus" (must be 'ephemeral' or '1h')`
	if err.Error() != expectedMsg {
		t.Errorf("unexpected error message: got %v, want %v", err.Error(), expectedMsg)
	}
}

func TestPrefixCache_InvalidTTL_EmptyWithPrefixCacheDisabled(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         false,
			CacheControlTTL: "bogus",
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Errorf("expected no error when prefix cache is disabled, got: %v", err)
	}
}

func TestPrefixCache_CacheDefaultsApplied(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled: true,
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.CacheSystem == nil || !*cfg.PrefixCache.CacheSystem {
		t.Error("expected CacheSystem to default to true")
	}
	if cfg.PrefixCache.CacheTools == nil || !*cfg.PrefixCache.CacheTools {
		t.Error("expected CacheTools to default to true")
	}
	if cfg.PrefixCache.CacheMessages == nil || !*cfg.PrefixCache.CacheMessages {
		t.Error("expected CacheMessages to default to true")
	}
	if cfg.PrefixCache.CacheControlTTL != "ephemeral" {
		t.Errorf("expected CacheControlTTL to default to 'ephemeral', got %q", cfg.PrefixCache.CacheControlTTL)
	}
	if cfg.PrefixCache.CacheSystemTTL == nil || *cfg.PrefixCache.CacheSystemTTL != "ephemeral" {
		t.Errorf("expected CacheSystemTTL to default to 'ephemeral', got %v", cfg.PrefixCache.CacheSystemTTL)
	}
	if cfg.PrefixCache.CacheToolsTTL == nil || *cfg.PrefixCache.CacheToolsTTL != "ephemeral" {
		t.Errorf("expected CacheToolsTTL to default to 'ephemeral', got %v", cfg.PrefixCache.CacheToolsTTL)
	}
	if cfg.PrefixCache.CacheMessagesTTL == nil || *cfg.PrefixCache.CacheMessagesTTL != "ephemeral" {
		t.Errorf("expected CacheMessagesTTL to default to 'ephemeral', got %v", cfg.PrefixCache.CacheMessagesTTL)
	}
}

func TestPrefixCache_PerBreakpointTTLDefaults(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         true,
			CacheControlTTL: "1h",
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.CacheSystemTTL == nil || *cfg.PrefixCache.CacheSystemTTL != "1h" {
		t.Errorf("expected CacheSystemTTL to default to '1h', got %v", cfg.PrefixCache.CacheSystemTTL)
	}
	if cfg.PrefixCache.CacheToolsTTL == nil || *cfg.PrefixCache.CacheToolsTTL != "1h" {
		t.Errorf("expected CacheToolsTTL to default to '1h', got %v", cfg.PrefixCache.CacheToolsTTL)
	}
	if cfg.PrefixCache.CacheMessagesTTL == nil || *cfg.PrefixCache.CacheMessagesTTL != "1h" {
		t.Errorf("expected CacheMessagesTTL to default to '1h', got %v", cfg.PrefixCache.CacheMessagesTTL)
	}
}

func TestPrefixCache_InvalidPerBreakpointTTL_Rejected(t *testing.T) {
	bogus := "bogus"
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:         true,
			CacheControlTTL: "ephemeral",
			CacheSystemTTL:  &bogus,
		},
	}
	err := ApplyDefaults(cfg)
	if err == nil {
		t.Fatal("expected error for invalid per-breakpoint TTL 'bogus', got nil")
	}
}

func TestPrefixCache_InvalidTTLOrdering_Rejected(t *testing.T) {
	ephemeral := "ephemeral"
	oneHour := "1h"
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "messages 1h after ephemeral system",
			cfg: Config{PrefixCache: PrefixCacheConfig{
				Enabled:          true,
				CacheControlTTL:  "ephemeral",
				CacheSystemTTL:   &ephemeral,
				CacheMessagesTTL: &oneHour,
			}},
		},
		{
			name: "tools 1h after ephemeral system",
			cfg: Config{PrefixCache: PrefixCacheConfig{
				Enabled:         true,
				CacheControlTTL: "ephemeral",
				CacheSystemTTL:  &ephemeral,
				CacheToolsTTL:   &oneHour,
			}},
		},
		{
			name: "messages 1h after ephemeral tools",
			cfg: Config{PrefixCache: PrefixCacheConfig{
				Enabled:          true,
				CacheControlTTL:  "ephemeral",
				CacheToolsTTL:    &ephemeral,
				CacheMessagesTTL: &oneHour,
			}},
		},
		{
			name: "global 1h with ephemeral system override",
			cfg: Config{PrefixCache: PrefixCacheConfig{
				Enabled:         true,
				CacheControlTTL: "1h",
				CacheSystemTTL:  &ephemeral,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyDefaults(&tt.cfg)
			if err == nil {
				t.Fatal("expected error for invalid TTL ordering, got nil")
			}
		})
	}
}

func TestPrefixCache_ValidTTLOrdering_Accepted(t *testing.T) {
	ephemeral := "ephemeral"
	oneHour := "1h"
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:          true,
			CacheControlTTL:  "ephemeral",
			CacheSystemTTL:   &oneHour,
			CacheToolsTTL:    &oneHour,
			CacheMessagesTTL: &ephemeral,
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("expected no error for valid TTL ordering, got: %v", err)
	}
}

func TestPrefixCache_MergeTTLFields(t *testing.T) {
	base := &Config{PrefixCache: PrefixCacheConfig{Enabled: true}}
	oneHour := "1h"
	overlay := &Config{PrefixCache: PrefixCacheConfig{
		Enabled:          true,
		CacheSystemTTL:   &oneHour,
		CacheToolsTTL:    nil, // unset, should merge nil
		CacheMessagesTTL: &oneHour,
	}}

	mergePrefixCacheConfig(base, overlay)

	if base.PrefixCache.CacheSystemTTL == nil || *base.PrefixCache.CacheSystemTTL != "1h" {
		t.Errorf("CacheSystemTTL not merged: got %v", base.PrefixCache.CacheSystemTTL)
	}
	if base.PrefixCache.CacheToolsTTL != nil {
		t.Errorf("CacheToolsTTL should remain nil (unset in overlay): got %v", *base.PrefixCache.CacheToolsTTL)
	}
	if base.PrefixCache.CacheMessagesTTL == nil || *base.PrefixCache.CacheMessagesTTL != "1h" {
		t.Errorf("CacheMessagesTTL not merged: got %v", base.PrefixCache.CacheMessagesTTL)
	}
}

func TestPrefixCache_CacheModeDefaults(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled: true,
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.CacheMode == nil || *cfg.PrefixCache.CacheMode != CacheModeExplicit {
		t.Errorf("expected CacheMode to default to 'explicit', got %v", cfg.PrefixCache.CacheMode)
	}
}

func TestPrefixCache_CacheModeAutomaticForcesBlockMarkersOff(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:          true,
			CacheSystem:      PtrTo(true),
			CacheTools:       PtrTo(true),
			CacheMessages:    PtrTo(true),
			CacheControlTTL:  "1h",
		},
	}
	automatic := CacheModeAutomatic
	cfg.PrefixCache.CacheMode = &automatic
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.CacheSystem == nil || *cfg.PrefixCache.CacheSystem != false {
		t.Errorf("expected CacheSystem forced to false in automatic mode, got %v", cfg.PrefixCache.CacheSystem)
	}
	if cfg.PrefixCache.CacheTools == nil || *cfg.PrefixCache.CacheTools != false {
		t.Errorf("expected CacheTools forced to false in automatic mode, got %v", cfg.PrefixCache.CacheTools)
	}
	if cfg.PrefixCache.CacheMessages == nil || *cfg.PrefixCache.CacheMessages != false {
		t.Errorf("expected CacheMessages forced to false in automatic mode, got %v", cfg.PrefixCache.CacheMessages)
	}
}

func TestPrefixCache_CacheModeNilWhenDisabled(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled: false,
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.CacheMode != nil {
		t.Errorf("expected CacheMode to remain nil when prefix cache is disabled, got %v", cfg.PrefixCache.CacheMode)
	}
}

func TestPrefixCache_InvalidCacheModeRejected(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:  true,
			CacheMode: PtrTo("bogus"),
		},
	}
	err := ApplyDefaults(cfg)
	if err == nil {
		t.Fatal("expected error for invalid cache_mode 'bogus', got nil")
	}
	expectedMsg := `invalid cache_mode: "bogus" (must be ` + "`explicit`" + ` or ` + "`automatic`" + `)`
	if err.Error() != expectedMsg {
		t.Errorf("unexpected error message: got %v, want %v", err.Error(), expectedMsg)
	}
}

func TestPrefixCache_MergeCacheMode(t *testing.T) {
	base := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:   true,
			CacheMode: PtrTo(CacheModeExplicit),
		},
	}
	automatic := CacheModeAutomatic
	overlay := &Config{
		PrefixCache: PrefixCacheConfig{
			CacheMode: &automatic,
		},
	}
	mergePrefixCacheConfig(base, overlay)
	if base.PrefixCache.CacheMode == nil || *base.PrefixCache.CacheMode != CacheModeAutomatic {
		t.Errorf("CacheMode not merged: got %v", base.PrefixCache.CacheMode)
	}
}

func TestPrefixCache_OpenAIBreakpointDefaults(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled: true,
		},
	}
	err := ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("ApplyDefaults failed: %v", err)
	}
	if cfg.PrefixCache.OpenAIBreakpoint == nil || !*cfg.PrefixCache.OpenAIBreakpoint {
		t.Errorf("expected OpenAIBreakpoint to default to true, got %v", cfg.PrefixCache.OpenAIBreakpoint)
	}
	if cfg.PrefixCache.OpenAIMode == nil || *cfg.PrefixCache.OpenAIMode != OpenAIModeImplicit {
		t.Errorf("expected OpenAIMode to default to 'implicit', got %v", cfg.PrefixCache.OpenAIMode)
	}
}

func TestPrefixCache_InvalidOpenAIModeRejected(t *testing.T) {
	cfg := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:    true,
			OpenAIMode: PtrTo("bogus"),
		},
	}
	err := ApplyDefaults(cfg)
	if err == nil {
		t.Fatal("expected error for invalid openai_mode 'bogus', got nil")
	}
	expectedMsg := `invalid openai_mode: "bogus" (must be ` + "`implicit`" + ` or ` + "`explicit`" + `)`
	if err.Error() != expectedMsg {
		t.Errorf("unexpected error message: got %v, want %v", err.Error(), expectedMsg)
	}
}

func TestPrefixCache_MergeOpenAIFields(t *testing.T) {
	base := &Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:          true,
			OpenAIBreakpoint: PtrTo(false),
			OpenAIMode:       PtrTo(OpenAIModeImplicit),
		},
	}
	explicit := OpenAIModeExplicit
	overlay := &Config{
		PrefixCache: PrefixCacheConfig{
			OpenAIBreakpoint: PtrTo(true),
			OpenAIMode:       &explicit,
		},
	}
	mergePrefixCacheConfig(base, overlay)
	if base.PrefixCache.OpenAIBreakpoint == nil || !*base.PrefixCache.OpenAIBreakpoint {
		t.Errorf("OpenAIBreakpoint not merged: got %v", base.PrefixCache.OpenAIBreakpoint)
	}
	if base.PrefixCache.OpenAIMode == nil || *base.PrefixCache.OpenAIMode != OpenAIModeExplicit {
		t.Errorf("OpenAIMode not merged: got %v", base.PrefixCache.OpenAIMode)
	}
}

func TestEffectiveUpstreamTimeout(t *testing.T) {
	tests := []struct {
		name            string
		cfg             GovernanceConfig
		expectedSeconds int
	}{
		{"not set defaults to 300s (5 minutes)", GovernanceConfig{}, 300},
		{"set to 0 returns 0 (unlimited)", GovernanceConfig{UpstreamTimeoutSeconds: PtrTo(0)}, 0},
		{"set to 600 returns 600s", GovernanceConfig{UpstreamTimeoutSeconds: PtrTo(600)}, 600},
		{"set to negative value returns 0 (unlimited)", GovernanceConfig{UpstreamTimeoutSeconds: PtrTo(-1)}, 0},
		{"exactly 86400 passes through", GovernanceConfig{UpstreamTimeoutSeconds: PtrTo(86400)}, 86400},
		{"exceeds 24h capped to 86400", GovernanceConfig{UpstreamTimeoutSeconds: PtrTo(86401)}, 86400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EffectiveUpstreamTimeout()
			want := time.Duration(tt.expectedSeconds) * time.Second
			if got != want {
				t.Errorf("EffectiveUpstreamTimeout() = %v, want %v", got, want)
			}
		})
	}
}

func TestEffectiveStreamIdleTimeout(t *testing.T) {
	tests := []struct {
		name            string
		cfg             GovernanceConfig
		expectedSeconds int
	}{
		{"not set defaults to 300s (5 minutes)", GovernanceConfig{}, 300},
		{"set to 0 returns 0 (disabled)", GovernanceConfig{StreamIdleTimeoutSeconds: PtrTo(0)}, 0},
		{"set to 300 returns 300s", GovernanceConfig{StreamIdleTimeoutSeconds: PtrTo(300)}, 300},
		{"set to negative returns 0 (disabled)", GovernanceConfig{StreamIdleTimeoutSeconds: PtrTo(-1)}, 0},
		{"exactly 86400 passes through", GovernanceConfig{StreamIdleTimeoutSeconds: PtrTo(86400)}, 86400},
		{"exceeds 24h capped to 86400", GovernanceConfig{StreamIdleTimeoutSeconds: PtrTo(86401)}, 86400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EffectiveStreamIdleTimeout()
			want := time.Duration(tt.expectedSeconds) * time.Second
			if got != want {
				t.Errorf("EffectiveStreamIdleTimeout() = %v, want %v", got, want)
			}
		})
	}
}

func TestEffectiveThinkingStreamIdleTimeout(t *testing.T) {
	tests := []struct {
		name            string
		cfg             GovernanceConfig
		expectedSeconds int
	}{
		{"not set defaults to 600s", GovernanceConfig{}, 600},
		{"set to 0 returns 0 (disabled)", GovernanceConfig{ThinkingStreamIdleTimeoutSeconds: PtrTo(0)}, 0},
		{"set to 300 returns 300s", GovernanceConfig{ThinkingStreamIdleTimeoutSeconds: PtrTo(300)}, 300},
		{"set to negative returns 0 (disabled)", GovernanceConfig{ThinkingStreamIdleTimeoutSeconds: PtrTo(-1)}, 0},
		{"exactly 86400 passes through", GovernanceConfig{ThinkingStreamIdleTimeoutSeconds: PtrTo(86400)}, 86400},
		{"exceeds 24h capped to 86400", GovernanceConfig{ThinkingStreamIdleTimeoutSeconds: PtrTo(86401)}, 86400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EffectiveThinkingStreamIdleTimeout()
			want := time.Duration(tt.expectedSeconds) * time.Second
			if got != want {
				t.Errorf("EffectiveThinkingStreamIdleTimeout() = %v, want %v", got, want)
			}
		})
	}
}
