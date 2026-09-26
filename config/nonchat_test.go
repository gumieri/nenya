package config

import "testing"

func TestCompileNonChatModels(t *testing.T) {
	got, err := CompileNonChatModels(nil)
	if err != nil || got != nil {
		t.Fatalf("CompileNonChatModels(nil) = %v, %v; want nil, nil", got, err)
	}

	if _, invalidErr := CompileNonChatModels([]string{"("}); invalidErr == nil {
		t.Fatal("CompileNonChatModels() accepted an invalid pattern")
	}

	got, err = CompileNonChatModels([]string{`^jev-`})
	if err != nil {
		t.Fatalf("CompileNonChatModels() error = %v", err)
	}
	if len(got) != 1 || !got[0].MatchString("jev-1.13-free") {
		t.Fatalf("CompileNonChatModels() did not match jev-1.13-free")
	}
}

func TestProviderIsNonChatModel(t *testing.T) {
	var nilProvider *Provider
	if nilProvider.IsNonChatModel("jev-1.13") {
		t.Fatal("nil provider must report false")
	}

	re, err := CompileNonChatModels([]string{`^jev-`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := &Provider{Name: "zen", nonChatRE: re}

	if !p.IsNonChatModel("jev-1.13-free") {
		t.Error("expected jev-1.13-free to be non-chat")
	}
	if p.IsNonChatModel("kimi-k3") {
		t.Error("expected kimi-k3 to be a chat model")
	}

	empty := &Provider{Name: "zen"}
	if empty.IsNonChatModel("jev-1.13") {
		t.Error("provider without non_chat_models must report false")
	}
}

func TestAnyProviderServesAsChat(t *testing.T) {
	re, err := CompileNonChatModels([]string{`^jev-`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	zen := &Provider{Name: "zen", nonChatRE: re}
	other := &Provider{Name: "other"}

	tests := []struct {
		name      string
		providers map[string]*Provider
		id        string
		servable  []ServedModel
		want      bool
	}{
		{
			name:      "only non-chat provider serves it",
			providers: map[string]*Provider{"zen": zen},
			id:        "jev-1.13-free",
			servable:  []ServedModel{{Provider: "zen", Model: "jev-1.13-free"}},
			want:      false,
		},
		{
			name:      "chat model on the same provider",
			providers: map[string]*Provider{"zen": zen},
			id:        "kimi-k3",
			servable:  []ServedModel{{Provider: "zen", Model: "kimi-k3"}},
			want:      true,
		},
		{
			name:      "another provider serves it as chat",
			providers: map[string]*Provider{"zen": zen, "other": other},
			id:        "jev-1.13-free",
			servable: []ServedModel{
				{Provider: "zen", Model: "jev-1.13-free"},
				{Provider: "other", Model: "jev-1.13-free"},
			},
			want: true,
		},
		{
			name:      "no provider serves it (empty servable)",
			providers: map[string]*Provider{"zen": zen},
			id:        "jev-1.13-free",
			servable:  nil,
			want:      false,
		},
		{
			name:      "no providers configured and no servable",
			providers: map[string]*Provider{},
			id:        "jev-1.13-free",
			servable:  nil,
			want:      true,
		},
		{
			name:      "nil providers map",
			providers: nil,
			id:        "any",
			servable:  nil,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AnyProviderServesAsChat(tt.providers, tt.id, tt.servable); got != tt.want {
				t.Errorf("AnyProviderServesAsChat() = %v, want %v", got, tt.want)
			}
		})
	}
}
