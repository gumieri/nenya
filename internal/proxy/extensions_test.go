package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
)

func newExtensionTestProxy(t *testing.T, upstream *httptest.Server) (*Proxy, *gateway.NenyaGateway) {
	t.Helper()
	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(10), RatelimitMaxTPM: config.PtrTo(10000)},
	}
	providers := map[string]*config.Provider{
		"openai": {
			Name:          "openai",
			URL:           upstream.URL + "/v1/chat/completions",
			BaseURL:       upstream.URL + "/v1",
			APIKey:        "test-key",
			AuthStyle:     "bearer",
			TimeoutSeconds: 30,
		},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{"openai": "test-key"},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    providers,
		RateLimiter:  infra.NewRateLimiter(10, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)
	return proxy, gw
}

func TestHandleImagesGenerations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("expected /v1/images/generations, got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"created": 1234567890,
			"data": []interface{}{
				map[string]interface{}{
					"b64_json": "iVBORw0KGgoAAAANS",
					"revised_prompt": "A beautiful sunset",
				},
			},
		})
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"A beautiful sunset","n":1,"size":"1024x1024"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if _, ok := resp["data"]; !ok {
		t.Errorf("expected data field in response, got %v", resp)
	}
}

func TestHandleAudioTranscriptions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("expected /v1/audio/transcriptions, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "multipart/form-data") {
			t.Errorf("expected multipart/form-data, got %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"text": "Hello world, this is a transcription.",
		})
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	body := "--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"audio.mp3\"\r\nContent-Type: audio/mpeg\r\n\r\nfake audio data\r\n--boundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nwhisper-1\r\n--boundary--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if resp["text"] != "Hello world, this is a transcription." {
		t.Errorf("expected text response, got %v", resp["text"])
	}
}

func TestHandleAudioSpeech(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("expected /v1/audio/speech, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %s", ct)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("fake audio data"))
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"Hello world","voice":"alloy"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("expected audio/mpeg, got %s", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "fake audio data" {
		t.Errorf("expected fake audio data, got %s", string(body))
	}
}

func TestHandleModerations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/moderations" {
			t.Errorf("expected /v1/moderations, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "modr-abc123",
			"model": "text-moderation-latest",
			"results": []interface{}{
				map[string]interface{}{
					"flagged": false,
					"categories": map[string]interface{}{},
					"category_scores": map[string]interface{}{},
				},
			},
		})
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"This is a safe message"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if resp["id"] != "modr-abc123" {
		t.Errorf("expected id=modr-abc123, got %v", resp["id"])
	}
}

func TestHandleRerank(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("expected /v1/rerank, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "rerank-abc123",
			"results": []interface{}{
				map[string]interface{}{"index": 1, "relevance_score": 0.95},
				map[string]interface{}{"index": 0, "relevance_score": 0.85},
			},
		})
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"rerank-english-v2.0","query":"What is the capital of France?","documents":["Paris is the capital of France.","The Eiffel Tower is in Paris."]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if resp["id"] != "rerank-abc123" {
		t.Errorf("expected id=rerank-abc123, got %v", resp["id"])
	}
}

func TestHandleA2A(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/a2a" {
			t.Errorf("expected /v1/a2a, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agent_id": "agent-123",
			"status": "completed",
			"result": "Agent-to-agent communication successful",
		})
	}))
	defer upstream.Close()

	proxy, _ := newExtensionTestProxy(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/a2a", strings.NewReader(`{"agent_id":"agent-123","message":"Hello from agent A"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("expected status=completed, got %v", resp["status"])
	}
}

func TestHandleExtensions_TooLarge(t *testing.T) {
	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(10), RatelimitMaxTPM: config.PtrTo(10000)},
	}
	providers := map[string]*config.Provider{
		"openai": {
			Name:          "openai",
			URL:           "https://api.openai.com/v1/chat/completions",
			BaseURL:       "https://api.openai.com",
			APIKey:        "test-key",
			AuthStyle:     "bearer",
			TimeoutSeconds: 30,
		},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{"openai": "test-key"},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    providers,
		RateLimiter:  infra.NewRateLimiter(10, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	tests := []struct {
		name    string
		path    string
		body    string
		headers map[string]string
	}{
		{
			name: "images generations too large",
			path: "/v1/images/generations",
			body: `{"prompt":"A beautiful sunset"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
		{
			name: "moderations too large",
			path: "/v1/moderations",
			body: `{"input":"Test message"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer client-token")
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("expected 413, got %d", rec.Code)
			}
		})
	}
}

func TestHandleExtensions_NoProvider(t *testing.T) {
	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(10), RatelimitMaxTPM: config.PtrTo(10000)},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    map[string]*config.Provider{},
		RateLimiter:  infra.NewRateLimiter(10, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"test"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestHandleExtensions_PathTraversal(t *testing.T) {
	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(10), RatelimitMaxTPM: config.PtrTo(10000)},
	}
	providers := map[string]*config.Provider{
		"openai": {
			Name:          "openai",
			URL:           "https://api.openai.com/v1/chat/completions",
			BaseURL:       "https://api.openai.com",
			APIKey:        "test-key",
			AuthStyle:     "bearer",
			TimeoutSeconds: 30,
		},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{"openai": "test-key"},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    providers,
		RateLimiter:  infra.NewRateLimiter(10, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations/../etc/passwd", strings.NewReader(`{"prompt":"test"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleExtensions_RateLimitExceeded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(1), RatelimitMaxTPM: config.PtrTo(10000)},
	}
	providers := map[string]*config.Provider{
		"openai": {
			Name:          "openai",
			URL:           upstream.URL + "/v1/images/generations",
			BaseURL:       upstream.URL + "/v1",
			APIKey:        "test-key",
			AuthStyle:     "bearer",
			TimeoutSeconds: 30,
		},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{"openai": "test-key"},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    providers,
		RateLimiter:  infra.NewRateLimiter(1, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	makeRequest := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prompt":"test"}`))
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		return rec.Code
	}

	code1 := makeRequest("/v1/images/generations")
	if code1 != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", code1)
	}

	code2 := makeRequest("/v1/images/generations")
	if code2 != http.StatusTooManyRequests {
		t.Errorf("expected 429 for second request, got %d", code2)
	}
}

func TestHandleExtensions_Upstream5xxFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(10), RatelimitMaxTPM: config.PtrTo(10000), MaxRetryAttempts: 2},
	}
	providers := map[string]*config.Provider{
		"openai": {
			Name:          "openai",
			URL:           upstream.URL + "/v1/images/generations",
			BaseURL:       upstream.URL + "/v1",
			APIKey:        "test-key",
			AuthStyle:     "bearer",
			TimeoutSeconds: 30,
		},
	}
	logger := infra.SetupLogger(false)
	secrets := &config.SecretsConfig{
		ClientToken:  "client-token",
		ProviderKeys: map[string]string{"openai": "test-key"},
	}
	gw := &gateway.NenyaGateway{
		Config:       cfg,
		Secrets:      secrets,
		Client:       http.DefaultClient,
		Providers:    providers,
		RateLimiter:  infra.NewRateLimiter(10, 10000),
		Stats:        infra.NewUsageTracker(),
		Logger:       logger,
	}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"test"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}
