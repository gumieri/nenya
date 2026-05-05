package config

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateTFIDFQuerySource(t *testing.T) {
	tests := []struct {
		source string
		hasErr bool
	}{
		{"", false},
		{"prior_messages", false},
		{"self", false},
		{"invalid", true},
	}
	for _, tt := range tests {
		errs := validateTFIDFQuerySource(tt.source)
		got := len(errs) > 0
		if got != tt.hasErr {
			t.Errorf("validateTFIDFQuerySource(%q) errors=%v, want hasErr=%v", tt.source, errs, tt.hasErr)
		}
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestValidatePatternsToList(t *testing.T) {
	logger := testLogger()
	tests := []struct {
		name    string
		label   string
		pattern string
		isValid bool
	}{
		{"valid_regex", "test", `[a-z]+`, true},
		{"invalid_regex", "test", `[invalid`, false},
		{"empty_pattern", "test", ``, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validatePatternsToList(tt.label, []string{tt.pattern}, logger)
			got := len(errs) == 0
			if got != tt.isValid {
				t.Errorf("validatePatternsToList(%q, [%q]) = %v, want isValid=%v", tt.label, tt.pattern, errs, tt.isValid)
			}
		})
	}
}

func TestValidatePatterns(t *testing.T) {
	logger := testLogger()

	err := ValidatePatterns("test", []string{`[a-z]+`}, logger)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	err = ValidatePatterns("test", []string{`[invalid`}, logger)
	if err == nil {
		t.Error("expected error for invalid pattern")
	}
}

func TestValidateEntropyConfig(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		thresh  float64
		minTok  int
		wantErr bool
	}{
		{"disabled", false, 0, 0, false},
		{"valid", true, 4.0, 16, false},
		{"threshold_zero", true, 0, 16, true},
		{"threshold_too_high", true, 9.0, 16, true},
		{"min_token_too_low", true, 4.0, 4, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sf := BouncerConfig{
				EntropyEnabled:   tt.enabled,
				EntropyThreshold: tt.thresh,
				EntropyMinToken:  tt.minTok,
			}
			errs := validateEntropyConfig(sf)
			got := len(errs) > 0
			if got != tt.wantErr {
				t.Errorf("validateEntropyConfig(%+v) = %v, want err=%v", sf, errs, tt.wantErr)
			}
		})
	}
}

func TestApplyAuthHeader(t *testing.T) {
	tests := []struct {
		name      string
		authStyle string
		wantAuth  string
		wantGoog  string
		wantErr   bool
	}{
		{"bearer", "bearer", "Bearer mykey", "", false},
		{"bearer_goog", "bearer+x-goog", "Bearer mykey", "mykey", false},
		{"none", "none", "", "", false},
		{"unsupported", "custom", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "/", nil)
			provider := &Provider{
				APIKey:    "mykey",
				AuthStyle: tt.authStyle,
			}
			err := applyAuthHeader(req, provider)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := req.Header.Get("Authorization"); got != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tt.wantAuth)
			}
			if got := req.Header.Get("x-goog-api-key"); got != tt.wantGoog {
				t.Errorf("x-goog-api-key = %q, want %q", got, tt.wantGoog)
			}
		})
	}
}

func TestOllamaHealthURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:11434/api/generate", "http://localhost:11434/api/tags"},
		{"http://localhost:11434/v1/chat/completions", "http://localhost:11434/api/tags"},
		{"http://ollama:11434", "http://ollama:11434"},
	}
	for _, tt := range tests {
		got := OllamaHealthURL(tt.input)
		if got != tt.want {
			t.Errorf("OllamaHealthURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestValidateOllamaHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ok := validateOllamaHealth(context.Background(), server.URL)
	if !ok {
		t.Error("expected health check to pass")
	}
}

func TestValidateOllamaHealth_Failure(t *testing.T) {
	ok := validateOllamaHealth(context.Background(), "http://127.0.0.1:1/nonexistent")
	if ok {
		t.Error("expected health check to fail")
	}
}

func TestCalculateBackoff(t *testing.T) {
	d0 := calculateBackoff(0)
	if d0 < 500*time.Millisecond {
		t.Errorf("expected backoff >= 500ms, got %v", d0)
	}

	d1 := calculateBackoff(1)
	if d1 < d0 {
		t.Errorf("expected backoff to increase, got %v < %v", d1, d0)
	}

	d10 := calculateBackoff(10)
	if d10 > 10*time.Second {
		t.Errorf("expected backoff cap ~8s, got %v", d10)
	}
}

func TestDoWithRetry(t *testing.T) {
	attempts := 0
	err := doWithRetry(context.Background(), 3, func() error {
		attempts++
		if attempts < 2 {
			return errors.New("transient error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestDoWithRetry_AllFail(t *testing.T) {
	attempts := 0
	err := doWithRetry(context.Background(), 3, func() error {
		attempts++
		return errors.New("persistent error")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestDoWithRetry_NoRetry(t *testing.T) {
	attempts := 0
	err := doWithRetry(context.Background(), 1, func() error {
		attempts++
		return errors.New("error")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts)
	}
}

func TestDoWithRetry_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	err := doWithRetry(ctx, 3, func() error {
		attempts++
		return errors.New("error")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt (cancelled), got %d", attempts)
	}
}

func TestValidateConfigurationWithPing(t *testing.T) {
	logger := testLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(false),
		},
	}

	err := ValidateConfigurationWithPing(context.Background(), cfg, &SecretsConfig{}, logger, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfiguration_OllamaEnginePing(t *testing.T) {
	logger := testLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(true),
			Engine: EngineRef{
				Provider: "ollama",
			},
		},
		Providers: map[string]ProviderConfig{
			"ollama": {URL: server.URL},
		},
	}

	secrets := &SecretsConfig{
		ProviderKeys: map[string]string{
			"ollama": "",
		},
	}

	err := ValidateConfigurationWithPing(context.Background(), cfg, secrets, logger, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfiguration_OllamaEngine_NotReachable(t *testing.T) {
	logger := testLogger()

	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(true),
			Engine: EngineRef{
				Provider: "ollama",
			},
		},
		Providers: map[string]ProviderConfig{
			"ollama": {URL: "http://127.0.0.1:1/bogus"},
		},
	}

	secrets := &SecretsConfig{
		ProviderKeys: map[string]string{
			"ollama": "",
		},
	}

	err := ValidateConfigurationWithPing(context.Background(), cfg, secrets, logger, true)
	if err == nil {
		t.Error("expected error for unreachable Ollama engine")
	}
}

func TestCollectValidationErrors(t *testing.T) {
	logger := testLogger()
	cfg := &Config{}
	providers := make(map[string]*Provider)

	errs := collectValidationErrors(context.Background(), cfg, providers, false, logger)
	for _, e := range errs {
		t.Logf("validation error: %s", e)
	}
}

func TestValidateOllamaEngine_NoPing(t *testing.T) {
	logger := testLogger()
	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(true),
		},
	}
	providers := make(map[string]*Provider)

	err := validateOllamaEngine(context.Background(), cfg, providers, false, logger)
	if err != nil {
		t.Errorf("expected no error when ping disabled, got: %v", err)
	}
}

func TestValidateOllamaEngine_Disabled(t *testing.T) {
	logger := testLogger()
	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(false),
		},
	}
	providers := make(map[string]*Provider)

	err := validateOllamaEngine(context.Background(), cfg, providers, true, logger)
	if err != nil {
		t.Errorf("expected no error when bouncer disabled, got: %v", err)
	}
}

func TestValidateOllamaEngine_NoProvider(t *testing.T) {
	logger := testLogger()
	cfg := &Config{
		Bouncer: BouncerConfig{
			Enabled: PtrTo(true),
			Engine: EngineRef{
				Provider: "nonexistent",
			},
		},
	}
	providers := make(map[string]*Provider)

	err := validateOllamaEngine(context.Background(), cfg, providers, true, logger)
	if err != nil {
		t.Errorf("expected no error when provider missing, got: %v", err)
	}
}

func TestValidateWithMinimalRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	logger := testLogger()
	provider := &Provider{
		URL:       server.URL + "/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	err := validateWithMinimalRequest(provider, context.Background(), logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateWithMinimalRequest_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	logger := testLogger()
	provider := &Provider{
		URL:       server.URL + "/v1/chat/completions",
		APIKey:    "wrong-key",
		AuthStyle: "bearer",
	}

	err := validateWithMinimalRequest(provider, context.Background(), logger)
	if err == nil || !strings.Contains(err.Error(), "API key rejected") {
		t.Errorf("expected 'API key rejected' error, got: %v", err)
	}
}

func TestValidateWithMinimalRequest_UnexpectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	logger := testLogger()
	provider := &Provider{
		URL:       server.URL + "/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	err := validateWithMinimalRequest(provider, context.Background(), logger)
	if err == nil {
		t.Error("expected error for 404")
	}
}

func TestCloseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	closeBody(resp)
	closeBody(nil)
}
