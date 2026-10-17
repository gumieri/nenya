package config

import (
	"context"
	"testing"
)

// TestProviderRegistry_AnthropicIdempotencyHeader pins the built-in
// capability: the Anthropic Messages API accepts an Idempotency-Key
// (NENYA-165).
func TestProviderRegistry_AnthropicIdempotencyHeader(t *testing.T) {
	entry, ok := ProviderRegistry["anthropic"]
	if !ok {
		t.Fatal("anthropic missing from ProviderRegistry")
	}
	if entry.IdempotencyHeader == nil || *entry.IdempotencyHeader != "Idempotency-Key" {
		t.Fatalf("anthropic idempotency_header = %v, want Idempotency-Key", entry.IdempotencyHeader)
	}
	if cfg := entry.ToProviderConfig(); cfg.IdempotencyHeader == nil || *cfg.IdempotencyHeader != "Idempotency-Key" {
		t.Fatalf("ToProviderConfig dropped the idempotency_header: %v", cfg.IdempotencyHeader)
	}
}

// TestMergeProviderConfig_IdempotencyHeader pins the tri-state merge: nil
// inherits the built-in, an explicit empty string opts out, and any other
// value replaces the built-in name.
func TestMergeProviderConfig_IdempotencyHeader(t *testing.T) {
	builtIn := ProviderConfig{IdempotencyHeader: PtrTo("Idempotency-Key")}

	tests := []struct {
		name string
		user *string
		want string
	}{
		{name: "unset inherits", user: nil, want: "Idempotency-Key"},
		{name: "empty opts out", user: PtrTo(""), want: ""},
		{name: "custom wins", user: PtrTo("X-Idempotency"), want: "X-Idempotency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := mergeProviderConfig(ProviderConfig{IdempotencyHeader: tt.user}, builtIn)
			if merged.IdempotencyHeader == nil {
				t.Fatalf("merged idempotency_header is nil, want %q", tt.want)
			}
			if *merged.IdempotencyHeader != tt.want {
				t.Fatalf("merged idempotency_header = %q, want %q", *merged.IdempotencyHeader, tt.want)
			}
		})
	}
}

// TestMergeConfig_IdempotencyHeaderLayering pins the config.d tri-state: a
// base file's explicit "" opt-out survives a drop-in that omits the field, and
// a drop-in that sets the field wins.
func TestMergeConfig_IdempotencyHeaderLayering(t *testing.T) {
	t.Run("a drop-in that omits the field keeps the base opt-out", func(t *testing.T) {
		base := &Config{Providers: map[string]ProviderConfig{"anthropic": {IdempotencyHeader: PtrTo("")}}}
		overlay := &Config{Providers: map[string]ProviderConfig{"anthropic": {}}}
		mergeConfig(base, overlay)
		got := base.Providers["anthropic"].IdempotencyHeader
		if got == nil || *got != "" {
			t.Fatalf("idempotency_header = %v, want the base opt-out preserved", got)
		}
	})

	t.Run("a drop-in value wins", func(t *testing.T) {
		base := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo("Idempotency-Key")}}}
		overlay := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo("X-Idempotency")}}}
		mergeConfig(base, overlay)
		got := base.Providers["p"].IdempotencyHeader
		if got == nil || *got != "X-Idempotency" {
			t.Fatalf("idempotency_header = %v, want the drop-in value", got)
		}
	})
}

// TestResolveProviders_IdempotencyHeader pins the runtime resolution: the
// resolved string is what the dispatch reads, and the opt-out survives.
func TestResolveProviders_IdempotencyHeader(t *testing.T) {
	// The merge (and the anthropic built-in) is applied by the loader; each
	// case applies it explicitly, as the loader does at startup.
	resolve := func(t *testing.T, providers map[string]ProviderConfig) map[string]*Provider {
		t.Helper()
		cfg := Config{Providers: providers}
		applyBuiltInProviders(&cfg)
		return ResolveProviders(&cfg, &SecretsConfig{})
	}

	t.Run("a user anthropic entry inherits the built-in", func(t *testing.T) {
		got := resolve(t, map[string]ProviderConfig{"anthropic": {}})["anthropic"].IdempotencyHeader
		if got != "Idempotency-Key" {
			t.Fatalf("inherited idempotency_header = %q, want the built-in Idempotency-Key", got)
		}
	})

	t.Run("an explicit empty string opts out", func(t *testing.T) {
		got := resolve(t, map[string]ProviderConfig{
			"anthropic": {IdempotencyHeader: PtrTo("")},
		})["anthropic"].IdempotencyHeader
		if got != "" {
			t.Fatalf("opted-out idempotency_header = %q, want empty", got)
		}
	})

	t.Run("a custom provider declares its own", func(t *testing.T) {
		got := resolve(t, map[string]ProviderConfig{
			"custom": {AuthStyle: "none", IdempotencyHeader: PtrTo("X-Idempotency")},
		})["custom"].IdempotencyHeader
		if got != "X-Idempotency" {
			t.Fatalf("custom idempotency_header = %q, want X-Idempotency", got)
		}
	})
}

// TestValidateProviderOptions_IdempotencyHeader pins the load-time guard: the
// header name is written straight onto an outbound request, so a malformed,
// non-canonical, or gateway-owned one is rejected at load — including on the
// paths that do not resolve providers (the SIGHUP reload).
func TestValidateProviderOptions_IdempotencyHeader(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{name: "unset", header: ""},
		{name: "canonical", header: "Idempotency-Key"},
		{name: "custom token", header: "X-Idempotency-Key.1"},
		{name: "space is invalid", header: "Idempotency Key", wantErr: true},
		{name: "colon is invalid", header: "Idempotency:Key", wantErr: true},
		{name: "newline is invalid", header: "Idempotency\nKey", wantErr: true},
		{name: "non-canonical is invalid", header: "idempotency-key", wantErr: true},
		{name: "gateway-owned header is invalid", header: "Authorization", wantErr: true},
		{name: "transport header is invalid", header: "Content-Type", wantErr: true},
		{name: "session header is invalid", header: "Session-Id", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo(tt.header)}}}
			errs := validateProviderOptions(cfg)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("expected a validation error for %q", tt.header)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("unexpected validation errors: %v", errs)
			}
		})
	}

	t.Run("every gateway-owned header is rejected", func(t *testing.T) {
		// The list can drift from the dispatch sets (config cannot import
		// internal/adapter); this pins that each entry is actually rejected.
		for header := range reservedProviderHeaders {
			cfg := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo(header)}}}
			if errs := validateProviderOptions(cfg); len(errs) == 0 {
				t.Errorf("idempotency_header %q must be rejected as gateway-owned", header)
			}
		}
	})

	t.Run("accepts the canonical spelling", func(t *testing.T) {
		cfg := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo("Idempotency-Key")}}}
		if errs := validateProviderOptions(cfg); len(errs) != 0 {
			t.Fatalf("the canonical default must be accepted: %v", errs)
		}
	})

	t.Run("runs without provider resolution (reload path)", func(t *testing.T) {
		// collectValidationErrors with pingProviders=false must still catch it.
		cfg := &Config{Providers: map[string]ProviderConfig{"p": {IdempotencyHeader: PtrTo("Session-Id")}}}
		errs := collectValidationErrors(context.Background(), cfg, nil, false, testLogger())
		if len(errs) == 0 {
			t.Fatal("expected the reload-path validation to reject a gateway-owned header")
		}
	})
}
