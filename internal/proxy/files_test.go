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

func TestFilesRouteRegistration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"object": "list"})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	t.Run("GET /v1/files", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
		req.Header.Set("Authorization", "Bearer client-token")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected %d, got %d\nBody: %s", http.StatusOK, w.Code, w.Body.String())
		}
	})

	t.Run("POST /v1/files", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(`{"purpose":"fine-tune"}`))
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected %d, got %d\nBody: %s", http.StatusOK, w.Code, w.Body.String())
		}
	})
}

func TestHandleFiles_POST_JSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/files" {
			t.Errorf("expected /v1/files, got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "file-abc123", "object": "file", "bytes": 1200})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(`{"purpose":"fine-tune","file":"data:text/plain;base64,SGVsbG8gd29ybGQh"}`))
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
	if resp["id"] != "file-abc123" {
		t.Errorf("expected id=file-abc123, got %v", resp["id"])
	}
}

func TestHandleFiles_POST_Multipart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/files" {
			t.Errorf("expected /v1/files, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct == "" || !strings.Contains(ct, "multipart/form-data") {
			t.Errorf("expected multipart/form-data, got %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "file-multipart-xyz", "object": "file", "bytes": 500})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	body := "--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"test.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n{\"prompt\":\"hello\"}\r\n--boundary--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(body))
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
	if resp["id"] != "file-multipart-xyz" {
		t.Errorf("expected id=file-multipart-xyz, got %v", resp["id"])
	}
}

func TestHandleFiles_TooLarge(t *testing.T) {
	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 1}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: "https://api.openai.com/v1/chat/completions", BaseURL: "https://api.openai.com", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(`{"purpose":"fine-tune"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}

func TestHandleFiles_EmptyBody(t *testing.T) {
	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: "https://api.openai.com/v1/chat/completions", BaseURL: "https://api.openai.com", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d", rec.Code)
	}
}

func TestHandleFiles_GET_List(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/files" {
			t.Errorf("expected /v1/files, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{"id": "file-abc", "object": "file", "bytes": 100},
				map[string]interface{}{"id": "file-def", "object": "file", "bytes": 200},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	data, ok := resp["data"].([]interface{})
	if !ok || len(data) != 2 {
		t.Errorf("expected 2 files, got %v", resp["data"])
	}
}

func TestHandleFiles_GET_ByID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files/file-abc123" {
			t.Errorf("expected /v1/files/file-abc123, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "file-abc123", "object": "file", "bytes": 500})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/files/file-abc123", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleFiles_DELETE_ByID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files/file-abc123" {
			t.Errorf("expected /v1/files/file-abc123, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodDelete, "/v1/files/file-abc123", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
}

func TestHandleFiles_GET_Content(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files/file-abc123/content" {
			t.Errorf("expected /v1/files/file-abc123/content, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/jsonl")
		w.Write([]byte("{\"prompt\":\"hello\"}\n"))
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/files/file-abc123/content", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/jsonl" {
		t.Errorf("expected application/jsonl, got %s", ct)
	}
}

func TestHandleBatches_POST(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/batches" {
			t.Errorf("expected /v1/batches, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "batch-abc", "object": "batch", "status": "validating"})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(`{"input_file_id":"file-abc"}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("JSON decode: %v, body: %s", err, string(body))
	}
	if resp["id"] != "batch-abc" {
		t.Errorf("expected id=batch-abc, got %v", resp["id"])
	}
}

func TestHandleBatches_GET_List(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/batches" {
			t.Errorf("expected /v1/batches, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{"id": "batch-1", "status": "completed"},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/batches", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	data, ok := resp["data"].([]interface{})
	if !ok || len(data) != 1 {
		t.Errorf("expected 1 batch, got %v", resp["data"])
	}
}

func TestHandleBatches_GET_ByID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/batches/batch-xyz" {
			t.Errorf("expected /v1/batches/batch-xyz, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "batch-xyz", "status": "completed"})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/batches/batch-xyz", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleBatches_Cancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/batches/batch-xyz/cancel" {
			t.Errorf("expected /v1/batches/batch-xyz/cancel, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "batch-xyz", "status": "cancelling"})
	}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{MaxBodyBytes: 10 * 1024 * 1024}, Governance: config.GovernanceConfig{RatelimitMaxRPM: 10, RatelimitMaxTPM: 10000}}
	providers := map[string]*config.Provider{"openai": {Name: "openai", URL: upstream.URL + "/v1/chat/completions", BaseURL: upstream.URL + "/v1", APIKey: "test-key", AuthStyle: "bearer", TimeoutSeconds: 30}}
	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{Config: cfg, Secrets: &config.SecretsConfig{ClientToken: "client-token"}, Client: http.DefaultClient, Providers: providers, RateLimiter: infra.NewRateLimiter(10, 10000), Stats: infra.NewUsageTracker(), Logger: logger}
	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/batches/batch-xyz/cancel", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
