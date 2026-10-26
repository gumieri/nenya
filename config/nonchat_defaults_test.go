package config

import (
	"reflect"
	"testing"
)

// TestApplyDefaults_NonChatModelsProviderAgnostic pins the NENYA-180 built-in
// default: the well-known typed-decision model families are classified as
// non-chat on EVERY provider, not only on the built-in zen entry, so a provider
// that starts advertising a decision-only model (OpenRouter, TypeSafe direct,
// another Jev reseller) cannot expose it as chat-routable without operator
// action. A provider-level list replaces the defaults: the field is
// presence-based (omitted = inherit, explicit list including `[]` = the
// operator's intent).
func TestApplyDefaults_NonChatModelsProviderAgnostic(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{
		// An operator-defined provider with no non_chat_models of its own.
		"mygateway": {URL: "https://gw.example/v1/chat/completions", AuthStyle: "bearer"},
		// A provider that classifies its own decision models: the explicit
		// list REPLACES the defaults (it is not unioned).
		"legacy": {URL: "https://legacy.example/v1/chat/completions", AuthStyle: "bearer", NonChatModels: []string{`^legacy-decision-`}},
		// An EXPLICIT empty list is the opt-out: JSON distinguishes it from an
		// omitted field (nil), so this provider classifies nothing.
		"chatonly": {URL: "https://chatonly.example/v1/chat/completions", AuthStyle: "bearer", NonChatModels: []string{}},
		// A pattern that matches nothing is the other opt-out spelling.
		"never": {URL: "https://never.example/v1/chat/completions", AuthStyle: "bearer", NonChatModels: []string{`^$`}},
	}}
	if err := ApplyDefaults(cfg); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}

	if got := cfg.Providers["mygateway"].NonChatModels; !reflect.DeepEqual(got, defaultNonChatModelsCopy()) {
		t.Errorf("mygateway non_chat_models = %v, want the provider-agnostic defaults %v", got, defaultNonChatModelsCopy())
	}
	if got := cfg.Providers["legacy"].NonChatModels; !reflect.DeepEqual(got, []string{`^legacy-decision-`}) {
		t.Errorf("legacy non_chat_models = %v, want its own list (replacement, not union)", got)
	}
	if got := cfg.Providers["chatonly"].NonChatModels; len(got) != 0 {
		t.Errorf("an explicit empty list must stay empty (opt-out), got %v", got)
	}
	if got := cfg.Providers["never"].NonChatModels; !reflect.DeepEqual(got, []string{`^$`}) {
		t.Errorf("never non_chat_models = %v, want the opt-out pattern", got)
	}
	// The built-in zen entry keeps exactly its own declared classification (the
	// built-in merge must not be clobbered by the provider-agnostic default).
	if got, want := cfg.Providers["zen"].NonChatModels, BuiltInProviders()["zen"].NonChatModels; !reflect.DeepEqual(got, want) {
		t.Errorf("built-in zen non_chat_models = %v, want its declared %v", got, want)
	}

	// The resolved runtime provider carries the patterns, so the catalog merge,
	// the discovery filter, and the chat guard all see them.
	providers := ResolveProviders(cfg, &SecretsConfig{ProviderKeys: map[string]string{"mygateway": "k"}})
	gw := providers["mygateway"]
	if gw == nil {
		t.Fatal("mygateway did not resolve")
	}
	if !gw.IsNonChatModel("jev-1.13-free") {
		t.Error("a provider-agnostic default must classify jev-1.13-free as non-chat on an operator-defined provider")
	}
	if gw.IsNonChatModel("chat-model-1") {
		t.Error("the default must not classify an ordinary model as non-chat")
	}
	// Replacement semantics, behaviourally: a provider with its own list does
	// NOT classify the default family, so a legitimate `jev-` chat model stays
	// chat-routable there.
	legacy := providers["legacy"]
	if legacy == nil {
		t.Fatal("legacy did not resolve")
	}
	if legacy.IsNonChatModel("jev-1.13-free") {
		t.Error("an explicit list must replace the defaults (legacy must not inherit ^jev-)")
	}
	chatonly := providers["chatonly"]
	if chatonly == nil {
		t.Fatal("chatonly did not resolve")
	}
	if chatonly.IsNonChatModel("jev-1.13-free") {
		t.Error("an explicit empty list must opt the provider out of the classification")
	}
}

// TestDefaultNonChatModelsCompile pins that every provider-agnostic default is
// a valid RE2 pattern: an invalid one would be skipped (or, on the resolve path,
// drop the whole provider), silently disabling the classification.
func TestDefaultNonChatModelsCompile(t *testing.T) {
	patterns := defaultNonChatModelsCopy()
	if len(patterns) == 0 {
		t.Fatal("DefaultNonChatModels is empty: decision-only models would be chat-routable on every provider without an explicit list")
	}
	compiled, err := CompileNonChatModels(patterns)
	if err != nil {
		t.Fatalf("DefaultNonChatModels does not compile: %v", err)
	}
	matches := func(id string) bool {
		for _, re := range compiled {
			if re.MatchString(id) {
				return true
			}
		}
		return false
	}
	// The patterns must match the documented decision-only family.
	for _, id := range []string{"jev-1.13-free", "jev-2", "jev-1.13"} {
		if !matches(id) {
			t.Errorf("default patterns must classify %q as non-chat", id)
		}
	}
	// ... and must NOT match any static-registry model: a default that
	// classified a legitimate model would silently remove it from /v1/models
	// and 400 every chat request for it.
	for id := range ModelRegistry {
		if matches(id) {
			t.Errorf("the provider-agnostic default classifies the static model %q as non-chat", id)
		}
	}
	// The accessor returns a copy, so a caller cannot mutate the classification
	// for every provider.
	copied := defaultNonChatModelsCopy()
	if len(copied) > 0 {
		copied[0] = ".*"
		if reflect.DeepEqual(defaultNonChatModelsCopy(), copied) {
			t.Error("DefaultNonChatModels must return a copy")
		}
	}
}
