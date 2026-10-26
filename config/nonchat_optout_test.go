package config

import "testing"

// TestMergeConfig_EmptyListOptOut pins AUD-46 (NENYA-255): the documented
// non_chat_models semantics ("an explicit list, INCLUDING AN EMPTY ONE, is
// the operator's intent" — mergeProviderRouting) must be expressible through
// the conf.d drop-in merge too. Pre-fix mergeValue ignored empty slices, so a
// drop-in's non_chat_models: [] could never clear a base list, and
// ApplyNonChatDefaults then re-filled the defaults: the empty-list opt-out
// was inexpressible.
func TestMergeConfig_EmptyListOptOut(t *testing.T) {
	t.Run("a drop-in empty non_chat_models clears the base list", func(t *testing.T) {
		base := &Config{Providers: map[string]ProviderConfig{"p": {NonChatModels: []string{"^jev-"}}}}
		overlay := &Config{Providers: map[string]ProviderConfig{"p": {NonChatModels: []string{}}}}
		mergeConfig(base, overlay)
		got := base.Providers["p"].NonChatModels
		if got == nil || len(got) != 0 {
			t.Fatalf("non_chat_models = %v, want the explicit empty list (the opt-out)", got)
		}
	})

	t.Run("an absent field keeps the base list", func(t *testing.T) {
		base := &Config{Providers: map[string]ProviderConfig{"p": {NonChatModels: []string{"^jev-"}}}}
		overlay := &Config{Providers: map[string]ProviderConfig{"p": {}}}
		mergeConfig(base, overlay)
		got := base.Providers["p"].NonChatModels
		if len(got) != 1 || got[0] != "^jev-" {
			t.Fatalf("non_chat_models = %v, want the base list preserved (absent is not a clear)", got)
		}
	})

	t.Run("the opt-out survives ApplyNonChatDefaults", func(t *testing.T) {
		cfg := &Config{Providers: map[string]ProviderConfig{"p": {NonChatModels: []string{}}}}
		ApplyNonChatDefaults(cfg)
		if got := cfg.Providers["p"].NonChatModels; len(got) != 0 {
			t.Fatalf("non_chat_models = %v, want the defaults NOT re-filled over an explicit empty list", got)
		}
	})

	t.Run("an unset provider still receives the defaults", func(t *testing.T) {
		cfg := &Config{Providers: map[string]ProviderConfig{"p": {}}}
		ApplyNonChatDefaults(cfg)
		if got := cfg.Providers["p"].NonChatModels; len(got) == 0 {
			t.Fatal("defaults must still fill an unset list (the opt-out is opt-IN only)")
		}
	})
}
