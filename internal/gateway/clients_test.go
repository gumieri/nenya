package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nenya/config"
)

func TestBuildProviderClients(t *testing.T) {
	base := newUpstreamTransport(config.DefaultResponseHeaderTimeoutSeconds * time.Second)
	providers := map[string]*config.Provider{
		"default":  {Name: "default"},
		"explicit": {Name: "explicit", ResponseHeaderTimeoutSeconds: 120},
		"fallback": {Name: "fallback", TimeoutSeconds: 90},
	}

	clients := buildProviderClients(base, providers)

	if len(clients) != 2 {
		t.Fatalf("expected 2 dedicated clients, got %d: %v", len(clients), clients)
	}

	custom, ok := clients["explicit"]
	if !ok {
		t.Fatal("expected dedicated client for provider with response_header_timeout_seconds")
	}
	tr, ok := custom.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", custom.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("explicit ResponseHeaderTimeout = %v, want 120s", tr.ResponseHeaderTimeout)
	}
	if tr == base {
		t.Error("expected cloned transport, got base transport")
	}

	fallback, ok := clients["fallback"]
	if !ok {
		t.Fatal("expected dedicated client for provider falling back to timeout_seconds")
	}
	ftr := fallback.Transport.(*http.Transport)
	if ftr.ResponseHeaderTimeout != 90*time.Second {
		t.Errorf("fallback ResponseHeaderTimeout = %v, want 90s", ftr.ResponseHeaderTimeout)
	}
}

func TestUpstreamTransportResponseHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newUpstreamTransport(50 * time.Millisecond)}
	start := time.Now()
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected timeout awaiting response headers, got success")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("timeout fired after %v, want ~50ms", elapsed)
	}
}

func TestClientFor(t *testing.T) {
	defaultClient := &http.Client{Timeout: 1 * time.Second}
	ollamaClient := &http.Client{Timeout: 2 * time.Second}
	providerClient := &http.Client{Timeout: 3 * time.Second}

	gw := &NenyaGateway{
		Client:          defaultClient,
		OllamaClient:    ollamaClient,
		ProviderClients: map[string]*http.Client{"zai-coding-plan": providerClient},
		Providers: map[string]*config.Provider{
			"ollama-local":    {Name: "ollama-local", ApiFormat: "ollama"},
			"zai-coding-plan": {Name: "zai-coding-plan"},
			"plain":           {Name: "plain"},
		},
	}

	tests := []struct {
		name         string
		providerName string
		want         *http.Client
	}{
		{"ollama-format uses ollama client", "ollama-local", ollamaClient},
		{"registered provider uses dedicated client", "zai-coding-plan", providerClient},
		{"default-timeout provider shares base client", "plain", defaultClient},
		{"unknown provider falls back to base client", "unknown", defaultClient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gw.ClientFor(tt.providerName); got != tt.want {
				t.Errorf("ClientFor(%q) = %v, want %v", tt.providerName, got, tt.want)
			}
		})
	}
}

func TestClientFor_UninitializedRegistry(t *testing.T) {
	gw := &NenyaGateway{Client: &http.Client{}}
	if got := gw.ClientFor("anything"); got != gw.Client {
		t.Errorf("ClientFor on gateway without registry should return base client, got %v", got)
	}
}
