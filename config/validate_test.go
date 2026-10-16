package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateProviderResponseHeaderTimeout(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		wantErr bool
		wantMsg string
	}{
		{"unset is valid", 0, false, ""},
		{"positive is valid", 120, false, ""},
		{"max 24h is valid", MaxTimeoutSeconds, false, ""},
		{"negative fails", -1, true, `providers["p"].response_header_timeout_seconds must be non-negative (set to 0 to fall back to timeout_seconds or the 30s default), got -1`},
		{"exceeds 24h fails", MaxTimeoutSeconds + 1, true, fmt.Sprintf(`providers["p"].response_header_timeout_seconds exceeds maximum allowed value (%d seconds / 24 hours), got %d`, MaxTimeoutSeconds, MaxTimeoutSeconds+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := map[string]*Provider{"p": {ResponseHeaderTimeoutSeconds: tt.seconds}}
			errs := validateProviders(context.Background(), providers, nil, testLogger())
			if tt.wantErr {
				if len(errs) != 1 {
					t.Fatalf("expected 1 error, got %v", errs)
				}
				if errs[0] != tt.wantMsg {
					t.Errorf("error = %q, want %q", errs[0], tt.wantMsg)
				}
				return
			}
			if len(errs) != 0 {
				t.Errorf("expected no errors, got %v", errs)
			}
		})
	}
}

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

func TestValidateAgentStrategies(t *testing.T) {
	valid := []string{"", "round-robin", "fallback", "sticky"}
	invalid := []string{"random", "sticky+", "ROUND-ROBIN"}
	for _, s := range valid {
		errs := validateAgentStrategies(map[string]AgentConfig{"a": {Strategy: s}})
		if len(errs) != 0 {
			t.Errorf("strategy %q should be valid, got %v", s, errs)
		}
	}
	for _, s := range invalid {
		errs := validateAgentStrategies(map[string]AgentConfig{"a": {Strategy: s}})
		if len(errs) == 0 {
			t.Errorf("strategy %q should be invalid", s)
		}
	}
}

func TestValidateStickySessionTTL(t *testing.T) {
	ok := map[string]AgentConfig{"a": {Strategy: "sticky", StickySessionTTLSeconds: 3600}}
	if errs := validateStickySessionTTL(ok); len(errs) != 0 {
		t.Errorf("valid TTL got errors: %v", errs)
	}
	neg := map[string]AgentConfig{"a": {Strategy: "sticky", StickySessionTTLSeconds: -1}}
	if errs := validateStickySessionTTL(neg); len(errs) == 0 {
		t.Error("negative TTL should be invalid")
	}
	exceed := map[string]AgentConfig{"a": {Strategy: "sticky", StickySessionTTLSeconds: 99999}}
	if errs := validateStickySessionTTL(exceed); len(errs) == 0 {
		t.Error("TTL above 24h should be invalid")
	}
	// NENYA-196: pins and affinity resolve for every strategy, so the TTL is
	// validated for non-sticky agents too.
	nonSticky := map[string]AgentConfig{"a": {Strategy: "round-robin", StickySessionTTLSeconds: -5}}
	if errs := validateStickySessionTTL(nonSticky); len(errs) == 0 {
		t.Error("TTL validation must apply to non-sticky agents as well")
	}
	nonStickyOK := map[string]AgentConfig{"a": {Strategy: "round-robin", StickySessionTTLSeconds: 3600}}
	if errs := validateStickySessionTTL(nonStickyOK); len(errs) != 0 {
		t.Errorf("valid non-sticky TTL got errors: %v", errs)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestValidateProviderConcurrencyLimits(t *testing.T) {
	ok := &Config{
		Providers: map[string]ProviderConfig{
			"zai-coding-plan": {
				MaxConcurrentRequests: 5,
				ModelConcurrency:      map[string]int{"glm-5.3": 5, "glm-5.3-flash": 50, "free": 0},
			},
		},
	}
	if errs := validateProviderRateLimits(ok); len(errs) != 0 {
		t.Errorf("valid concurrency config got errors: %v", errs)
	}

	negProvider := &Config{
		Providers:  map[string]ProviderConfig{"p": {MaxConcurrentRequests: -1}},
		Governance: GovernanceConfig{MaxConcurrentRequests: -2},
	}
	errs := validateProviderRateLimits(negProvider)
	if len(errs) != 2 {
		t.Fatalf("negative caps should yield 2 errors, got %v", errs)
	}

	negModel := &Config{
		Providers: map[string]ProviderConfig{"p": {ModelConcurrency: map[string]int{"m": -3}}},
	}
	if errs := validateProviderRateLimits(negModel); len(errs) != 1 {
		t.Fatalf("negative model limit should yield 1 error, got %v", errs)
	}

	emptyKey := &Config{
		Providers: map[string]ProviderConfig{"p": {ModelConcurrency: map[string]int{"": 5}}},
	}
	if errs := validateProviderRateLimits(emptyKey); len(errs) != 1 {
		t.Fatalf("empty model key should yield 1 error, got %v", errs)
	}
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
			req, _ := http.NewRequest("POST", "/", http.NoBody)
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
			if got := req.Header.Get("X-Goog-Api-Key"); got != tt.wantGoog {
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

	ok := validateOllamaHealth(context.Background(), server.URL, "", "")
	if !ok {
		t.Error("expected health check to pass")
	}
}

func TestValidateOllamaHealth_Failure(t *testing.T) {
	ok := validateOllamaHealth(context.Background(), "http://127.0.0.1:1/nonexistent", "", "")
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

func TestDoWithRetry_CanceledContext(t *testing.T) {
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
		t.Errorf("expected 1 attempt (canceled), got %d", attempts)
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

	err := validateWithMinimalRequest(context.Background(), provider, nil, logger)
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

	err := validateWithMinimalRequest(context.Background(), provider, nil, logger)
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

	err := validateWithMinimalRequest(context.Background(), provider, nil, logger)
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

func TestValidateUpstreamTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name       string
		v          *int
		wantErr    bool
		wantErrMsg string
	}{
		{"nil passes", nil, false, ""},
		{"zero passes", PtrTo(0), false, ""},
		{"positive passes", PtrTo(300), false, ""},
		{"large positive passes", PtrTo(86400), false, ""},
		{"very large positive passes", PtrTo(2147483647), false, ""},
		{"negative fails", PtrTo(-1), true, "governance.upstream_timeout_seconds must be non-negative (set to 0 for unlimited), got -1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateUpstreamTimeoutSeconds(tt.v)
			hasErr := len(errs) > 0
			if hasErr != tt.wantErr {
				t.Errorf("validateUpstreamTimeoutSeconds() = %v, want hasErr=%v", errs, tt.wantErr)
				return
			}
			if tt.wantErrMsg != "" && (len(errs) != 1 || errs[0] != tt.wantErrMsg) {
				t.Errorf("validateUpstreamTimeoutSeconds() = %v, want exact error %q", errs, tt.wantErrMsg)
			}
		})
	}
}

func TestValidateStreamIdleTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name       string
		v          *int
		wantErr    bool
		wantErrMsg string
	}{
		{"nil passes", nil, false, ""},
		{"zero passes", PtrTo(0), false, ""},
		{"positive passes", PtrTo(120), false, ""},
		{"large positive passes", PtrTo(86400), false, ""},
		{"negative fails", PtrTo(-1), true, "governance.stream_idle_timeout_seconds must be non-negative (set to 0 to disable stall detection), got -1"},
		{"exceeds 86400 fails", PtrTo(86401), true, "governance.stream_idle_timeout_seconds exceeds maximum allowed value (86400 seconds / 24 hours), got 86401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateStreamIdleTimeoutSeconds(tt.v)
			hasErr := len(errs) > 0
			if hasErr != tt.wantErr {
				t.Errorf("validateStreamIdleTimeoutSeconds() = %v, want hasErr=%v", errs, tt.wantErr)
				return
			}
			if tt.wantErrMsg != "" && (len(errs) != 1 || errs[0] != tt.wantErrMsg) {
				t.Errorf("validateStreamIdleTimeoutSeconds() = %v, want exact error %q", errs, tt.wantErrMsg)
			}
		})
	}
}

func TestValidateBootstrapBufferBytes(t *testing.T) {
	tests := []struct {
		name       string
		v          int
		wantErr    bool
		wantErrMsg string
	}{
		{"zero disables", 0, false, ""},
		{"256KB passes", 256 * 1024, false, ""},
		{"1MiB passes", 1 << 20, false, ""},
		{"negative fails", -1, true, "governance.stream_bootstrap_buffer_bytes must be non-negative (0 disables bootstrap buffering), got -1"},
		{"exceeds 1MiB fails", (1 << 20) + 1, true, fmt.Sprintf("governance.stream_bootstrap_buffer_bytes exceeds maximum allowed value (%d bytes), got %d", MaxBootstrapBufferBytes, (1<<20)+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateBootstrapBufferBytes(tt.v)
			if hasErr := len(errs) > 0; hasErr != tt.wantErr {
				t.Errorf("validateBootstrapBufferBytes(%d) = %v, want hasErr=%v", tt.v, errs, tt.wantErr)
				return
			}
			if tt.wantErrMsg != "" && (len(errs) != 1 || errs[0] != tt.wantErrMsg) {
				t.Errorf("validateBootstrapBufferBytes(%d) = %v, want exact error %q", tt.v, errs, tt.wantErrMsg)
			}
		})
	}
}

func TestValidateStickyProvider(t *testing.T) {
	valid := map[string]AgentConfig{
		"a": {Strategy: "fallback", StickyProvider: ""},
		"b": {Strategy: "fallback", StickyProvider: "off"},
		"c": {Strategy: "fallback", StickyProvider: "lenient"},
		"d": {Strategy: "fallback", StickyProvider: "strict"},
	}
	if errs := validateAgentStrategies(valid); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	invalid := map[string]AgentConfig{
		"a": {Strategy: "fallback", StickyProvider: "maybe"},
	}
	errs := validateAgentStrategies(invalid)
	if len(errs) != 1 || !strings.Contains(errs[0], "sticky_provider") {
		t.Fatalf("expected sticky_provider error, got %v", errs)
	}
}

func TestValidateCacheAware(t *testing.T) {
	valid := map[string]AgentConfig{
		"a": {Strategy: "fallback", CacheAware: ""},
		"b": {Strategy: "fallback", CacheAware: "off"},
		"c": {Strategy: "fallback", CacheAware: "auto"},
		"d": {Strategy: "fallback", CacheAware: "force"},
	}
	if errs := validateAgentStrategies(valid); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	invalid := map[string]AgentConfig{
		"a": {Strategy: "fallback", CacheAware: "sometimes"},
	}
	errs := validateAgentStrategies(invalid)
	if len(errs) != 1 || !strings.Contains(errs[0], "cache_aware") {
		t.Fatalf("expected cache_aware error, got %v", errs)
	}
}

func TestValidateModelAliases(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"bad-empty-key":   {ModelAliases: map[string]string{"": "x"}},
			"bad-empty-value": {ModelAliases: map[string]string{"claude-haiku-4.5": ""}},
			"good":            {ModelAliases: map[string]string{"claude-haiku-4.5": "claude-haiku-4-5"}},
		},
	}
	errs := validateProviderModelAliases(cfg.Providers)
	if len(errs) != 2 {
		t.Fatalf("expected 2 alias errors, got %v", errs)
	}
}

// newSpotlightTestConfig returns a minimal config suitable for
// validateSpotlightConfig: one agent entry and no spotlight block yet.
func newSpotlightTestConfig() *Config {
	return &Config{
		Agents: map[string]AgentConfig{
			"test": {},
		},
	}
}

func TestValidateSpotlightConfig(t *testing.T) {
	enabled, disabled, historyOff := true, false, false
	spot := func(mode string, maxBytes int) *SpotlightConfig {
		return &SpotlightConfig{Enabled: &enabled, Mode: mode, MaxToolResultBytes: maxBytes}
	}

	t.Run("valid global", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot(SpotlightModeDatamarking, 2048)
		cfg.Governance.Spotlight.HistoryEnabled = &enabled
		if errs := validateSpotlightConfig(cfg); len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot("bogus", 2048)
		errs := validateSpotlightConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "governance.spotlight.mode") {
			t.Errorf("expected mode error, got %v", errs)
		}
	})

	t.Run("global-only byte cap rejected on agent", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot(SpotlightModeDelimiters, 2048)
		agent := cfg.Agents["test"]
		agent.Spotlight = &SpotlightConfig{MaxToolResultBytes: 512}
		cfg.Agents["test"] = agent
		errs := validateSpotlightConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "max_tool_result_bytes is global-only") {
			t.Errorf("expected global-only error, got %v", errs)
		}
	})

	t.Run("agent override surface", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot(SpotlightModeDelimiters, 2048)
		agent := cfg.Agents["test"]
		agent.Spotlight = &SpotlightConfig{Enabled: &disabled, HistoryEnabled: &historyOff}
		cfg.Agents["test"] = agent
		errs := validateSpotlightConfig(cfg)
		if len(errs) != 0 {
			t.Errorf("expected agent enabled/history accepted, got %v", errs)
		}
		agent = cfg.Agents["test"]
		agent.Spotlight = &SpotlightConfig{Enabled: &disabled, HistoryEnabled: &enabled}
		cfg.Agents["test"] = agent
		errs = validateSpotlightConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "contradictory") {
			t.Errorf("expected contradictory error, got %v", errs)
		}
	})

	t.Run("negative global byte cap rejected", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot(SpotlightModeDelimiters, -1)
		errs := validateSpotlightConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "max_tool_result_bytes") {
			t.Errorf("expected negative-cap error, got %v", errs)
		}
	})

	t.Run("agent mode rejected as global-only", func(t *testing.T) {
		cfg := newSpotlightTestConfig()
		cfg.Governance.Spotlight = spot(SpotlightModeDelimiters, 2048)
		agent := cfg.Agents["test"]
		agent.Spotlight = &SpotlightConfig{Mode: SpotlightModeDatamarking}
		cfg.Agents["test"] = agent
		errs := validateSpotlightConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "mode is global-only") {
			t.Errorf("expected agent mode error, got %v", errs)
		}
	})
}

func TestValidateInjectionEscalation(t *testing.T) {
	enabled := true
	engine := &EngineRef{Provider: "stub", Model: "classifier"}
	mkCfg := func(esc *InjectionEscalationConfig) *Config {
		esc.Engine = engine
		return &Config{Governance: GovernanceConfig{
			Injection: &InjectionConfig{Enabled: &enabled, Escalation: esc},
		}}
	}

	t.Run("enabled requires engine", func(t *testing.T) {
		cfg := &Config{Governance: GovernanceConfig{
			Injection: &InjectionConfig{Enabled: &enabled, Escalation: &InjectionEscalationConfig{Enabled: &enabled, MinScore: 1}},
		}}
		errs := validateInjectionEscalation(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "requires engine") {
			t.Errorf("expected engine-required error, got %v", errs)
		}
	})

	t.Run("band ordering", func(t *testing.T) {
		cfg := mkCfg(&InjectionEscalationConfig{Enabled: &enabled, MinScore: 3, MaxScore: 3})
		errs := validateInjectionEscalation(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "greater than min_score") {
			t.Errorf("expected band-order error, got %v", errs)
		}
	})

	t.Run("negative budgets rejected", func(t *testing.T) {
		cfg := mkCfg(&InjectionEscalationConfig{Enabled: &enabled, PerRequestLimit: -1, MaxBytes: -1, MinScore: -1})
		errs := validateInjectionEscalation(cfg)
		if len(errs) != 3 {
			t.Errorf("expected 3 errors, got %v", errs)
		}
	})

	t.Run("disabled escalation not validated", func(t *testing.T) {
		cfg := &Config{Governance: GovernanceConfig{
			Injection: &InjectionConfig{Enabled: &enabled, Escalation: &InjectionEscalationConfig{MinScore: -5}},
		}}
		if errs := validateInjectionEscalation(cfg); len(errs) != 0 {
			t.Errorf("expected no validation while disabled, got %v", errs)
		}
	})

	t.Run("per-agent escalation rejected", func(t *testing.T) {
		cfg := &Config{Agents: map[string]AgentConfig{
			"test": {Injection: &InjectionConfig{Escalation: &InjectionEscalationConfig{}}},
		}}
		errs := validateInjectionConfig(cfg, nil)
		if len(errs) != 1 || !strings.Contains(errs[0], "escalation is global-only") {
			t.Errorf("expected global-only error, got %v", errs)
		}
	})
}

func TestValidateCanaryConfig(t *testing.T) {
	enabled := true
	t.Run("valid actions", func(t *testing.T) {
		for _, action := range []string{"", CanaryActionLog, CanaryActionBlock} {
			cfg := &Config{Governance: GovernanceConfig{
				Canary: &CanaryConfig{Enabled: &enabled, Action: action},
			}}
			if errs := validateCanaryConfig(cfg); len(errs) != 0 {
				t.Errorf("expected %q valid, got %v", action, errs)
			}
		}
	})
	t.Run("invalid action rejected", func(t *testing.T) {
		cfg := &Config{Governance: GovernanceConfig{
			Canary: &CanaryConfig{Enabled: &enabled, Action: "blok"},
		}}
		errs := validateCanaryConfig(cfg)
		if len(errs) != 1 || !strings.Contains(errs[0], "governance.canary.action") {
			t.Errorf("expected action error, got %v", errs)
		}
	})
	t.Run("nil config valid", func(t *testing.T) {
		if errs := validateCanaryConfig(&Config{}); len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})
}

func TestValidateMCPGuardConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "valid policy",
			cfg:  &Config{Governance: GovernanceConfig{MCPGuard: &MCPGuardConfig{URLPolicy: "log"}}},
		},
		{
			name:    "invalid policy",
			cfg:     &Config{Governance: GovernanceConfig{MCPGuard: &MCPGuardConfig{URLPolicy: "deny_all"}}},
			wantErr: "url_policy",
		},
		{
			name:    "negative cap",
			cfg:     &Config{Governance: GovernanceConfig{MCPGuard: &MCPGuardConfig{MaxArgBytes: -1}}},
			wantErr: "max_arg_bytes",
		},
		{
			name: "valid allowlist",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.internal", "10.0.0.5", "*.example.com"}},
			}},
		},
		{
			name: "invalid allowlist entry",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.*.example"}},
			}},
			wantErr: "allowed_hosts",
		},
		{
			name: "empty allowlist entry",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"  "}},
			}},
			wantErr: "empty entry",
		},
		{
			name: "single label numeric wildcard",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.1"}},
			}},
			wantErr: "must not be an IP or all-numeric suffix",
		},
		{
			name: "multi label numeric wildcard covers private ips",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.0.0.1"}},
			}},
			wantErr: "must not be an IP or all-numeric suffix",
		},
		{
			name: "hex numeric wildcard",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.0x7f"}},
			}},
			wantErr: "must not be an IP or all-numeric suffix",
		},
		{
			name: "ipv6 literal wildcard rejected",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.::1"}},
			}},
			wantErr: "allowed_hosts",
		},
		{
			name: "empty wildcard suffix rejected",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*."}},
			}},
			wantErr: "empty wildcard suffix",
		},
		{
			name: "hex-looking word hostname allowed",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {AllowedHosts: []string{"*.cafe", "*.cab"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := collectValidationErrors(context.Background(), tt.cfg, map[string]*Provider{}, false, testLogger())
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Errorf("unexpected errors: %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e, tt.wantErr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("errors %v missing %q", errs, tt.wantErr)
			}
		})
	}
}

func TestApplyMCPGuardDefaults(t *testing.T) {
	t.Run("materializes guard when MCP servers configured", func(t *testing.T) {
		cfg := &Config{MCPServers: map[string]MCPServerConfig{"s": {URL: "http://localhost/sse"}}}
		if err := ApplyDefaults(cfg); err != nil {
			t.Fatalf("ApplyDefaults: %v", err)
		}
		if cfg.Governance.MCPGuard == nil {
			t.Fatal("mcp_guard must materialize when MCP servers are configured")
		}
		if !cfg.Governance.MCPGuard.EffectiveEnabled() {
			t.Error("materialized guard must be enabled by default")
		}
		if cfg.Governance.MCPGuard.EffectiveMaxArgBytes() != 1<<20 {
			t.Errorf("max_arg_bytes = %d, want 1 MiB", cfg.Governance.MCPGuard.EffectiveMaxArgBytes())
		}
		if cfg.Governance.MCPGuard.EffectiveURLPolicy() != "deny_private" {
			t.Errorf("url_policy = %q, want deny_private", cfg.Governance.MCPGuard.EffectiveURLPolicy())
		}
	})

	t.Run("no MCP servers leaves guard absent", func(t *testing.T) {
		cfg := &Config{}
		if err := ApplyDefaults(cfg); err != nil {
			t.Fatalf("ApplyDefaults: %v", err)
		}
		if cfg.Governance.MCPGuard != nil {
			t.Error("guard must stay absent without MCP servers")
		}
	})
}

// TestValidateSecretsRejectsDuplicateTokens pins the NENYA-196 identity
// invariant: two enabled keys may not share a token (which key name is
// attributed, and which per-key limits apply, would be nondeterministic).
func TestValidateSecretsRejectsDuplicateTokens(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Name: "a", Token: "shared-token-1234567890", Roles: []string{"user"}, Enabled: true},
			"b": {Name: "b", Token: "shared-token-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	err := validateSecrets(secrets)
	if err == nil || !strings.Contains(err.Error(), "share the same token") {
		t.Fatalf("expected a duplicate-token error, got %v", err)
	}

	// Disabled duplicates are inert (they never serve traffic).
	disabled := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Name: "a", Token: "shared-token-1234567890", Roles: []string{"user"}, Enabled: true},
			"b": {Name: "b", Token: "shared-token-1234567890", Roles: []string{"user"}, Enabled: false},
		},
	}
	if err := validateSecrets(disabled); err != nil {
		t.Fatalf("disabled duplicate must be skipped, got %v", err)
	}
}

// TestValidateSecretsRejectsDuplicateNames pins the NENYA-196 identity rule
// for the field that actually drives per-key limits and attribution.
func TestValidateSecretsRejectsDuplicateNames(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Name: "shared-name", Token: "token-a-1234567890", Roles: []string{"user"}, Enabled: true},
			"b": {Name: "shared-name", Token: "token-b-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	err := validateSecrets(secrets)
	if err == nil || !strings.Contains(err.Error(), "share the same name") {
		t.Fatalf("expected a duplicate-name error, got %v", err)
	}

	// A missing name is refused too (it is the metric/log identity).
	nameless := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Token: "token-a-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	if err := validateSecrets(nameless); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected a missing-name error, got %v", err)
	}
	// A key may not claim the synthetic primary-token name.
	primaryName := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Name: "primary", Token: "token-a-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	if err := validateSecrets(primaryName); err == nil || !strings.Contains(err.Error(), "is reserved for the primary-token identity") {
		t.Fatalf("expected a reserved-name error, got %v", err)
	}

	// A key token equal to the primary client_token is refused: the primary is
	// checked first, so the entry would be unreachable and the caller would
	// silently authenticate as the admin-class primary.
	primaryClash := &SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys: map[string]ApiKey{
			"a": {Name: "a", Token: "primary-token-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	if err := validateSecrets(primaryClash); err == nil || !strings.Contains(err.Error(), "collides with client_token") {
		t.Fatalf("expected a client_token collision error, got %v", err)
	}
}

// TestValidateAgentMCPReferences pins the NENYA-196 load check: an agent
// referencing an unknown (or empty) MCP server name refuses the load instead
// of silently running without tools.
func TestValidateAgentMCPReferences(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]MCPServerConfig{"known": {URL: "http://localhost:9000"}},
		Agents: map[string]AgentConfig{
			"ok":      {MCP: &AgentMCPConfig{Servers: []string{"known"}}},
			"unknown": {MCP: &AgentMCPConfig{Servers: []string{"missing"}}},
			"empty":   {MCP: &AgentMCPConfig{Servers: []string{""}}},
			"no-mcp":  {},
		},
	}
	errs := validateAgentMCPReferences(cfg)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors (unknown + empty), got %v", errs)
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "unknown mcp_servers[\"missing\"]") || !strings.Contains(joined, "empty server name") {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// TestValidateSecretAgentScopes pins the NENYA-196 cross-document check: an
// API key scoping itself to a non-existent agent refuses the load, while the
// documented empty-list "all agents" grant stays allowed (with a warning).
func TestValidateSecretAgentScopes(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{"alpha": {}, "beta": {}}}
	logger := testLogger()

	ok := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"scoped": {Name: "scoped", Enabled: true, Roles: []string{"user"}, AllowedAgents: []string{"alpha", "beta"}},
		"all":    {Name: "all", Enabled: true, Roles: []string{"user"}},
	}}
	if errs := validateSecretAgentScopes(cfg, ok, logger); len(errs) != 0 {
		t.Fatalf("valid scopes rejected: %v", errs)
	}

	bad := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"scoped": {Name: "scoped", Enabled: true, Roles: []string{"user"}, AllowedAgents: []string{"ghost"}},
	}}
	errs := validateSecretAgentScopes(cfg, bad, logger)
	if len(errs) != 1 || !strings.Contains(errs[0], "unknown agent \"ghost\"") {
		t.Fatalf("expected an unknown-agent error, got %v", errs)
	}

	empty := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"scoped": {Name: "scoped", Enabled: true, Roles: []string{"user"}, AllowedAgents: []string{""}},
	}}
	if errs := validateSecretAgentScopes(cfg, empty, logger); len(errs) != 1 || !strings.Contains(errs[0], "empty agent name") {
		t.Fatalf("expected an empty-name error, got %v", errs)
	}

	// Disabled keys are skipped entirely (they can never serve traffic).
	disabled := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"off": {Name: "off", Enabled: false, AllowedAgents: []string{"ghost"}},
	}}
	if errs := validateSecretAgentScopes(cfg, disabled, logger); len(errs) != 0 {
		t.Fatalf("disabled keys must be skipped, got %v", errs)
	}

	// Admin keys are exempt: AuthorizeAgent returns true for admin before
	// consulting allowed_agents, so a stale list is inert for them.
	admin := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"root": {Name: "root", Enabled: true, Roles: []string{"admin"}, AllowedAgents: []string{"ghost"}},
	}}
	if errs := validateSecretAgentScopes(cfg, admin, logger); len(errs) != 0 {
		t.Fatalf("admin keys must be exempt from the reference check, got %v", errs)
	}

	// Auto-generated agents are created after validation, so the reference
	// check is skipped when discovery.auto_agents is enabled.
	auto := &Config{
		Agents:    cfg.Agents,
		Discovery: DiscoveryConfig{Enabled: PtrTo(true), AutoAgents: PtrTo(true)},
	}
	if errs := validateSecretAgentScopes(auto, bad, logger); len(errs) != 0 {
		t.Fatalf("auto-agents must skip the reference check, got %v", errs)
	}
}

// TestValidateMCPServerConfigs pins the NENYA-183 MCP origin policy: absolute
// http(s) with a host, no embedded credentials, and no plaintext http to a
// remote host.
func TestValidateMCPServerConfigs(t *testing.T) {
	tests := []struct {
		name   string
		server MCPServerConfig
		want   string // expected substring, "" = must be valid
	}{
		{"https remote", MCPServerConfig{URL: "https://mcp.example.com/sse"}, ""},
		{"http loopback", MCPServerConfig{URL: "http://127.0.0.1:8080/sse"}, ""},
		{"http localhost name", MCPServerConfig{URL: "http://localhost:8080/sse"}, ""},
		{"http private", MCPServerConfig{URL: "http://10.1.2.3:8080/sse"}, ""},
		{"http unspecified v4", MCPServerConfig{URL: "http://0.0.0.0:8080/sse"}, ""},
		{"http unspecified v6", MCPServerConfig{URL: "http://[::]:8080/sse"}, ""},
		{"http remote", MCPServerConfig{URL: "http://mcp.example.com/sse"}, "plaintext http is only allowed for local hosts"},
		{"http link-local metadata", MCPServerConfig{URL: "http://169.254.169.254/sse"}, "plaintext http is only allowed for local hosts"},
		{"bad scheme", MCPServerConfig{URL: "ftp://mcp.example.com/sse"}, "unsupported scheme"},
		{"missing host", MCPServerConfig{URL: "https:///sse"}, "missing host"},
		{"port without host", MCPServerConfig{URL: "http://:8443/sse"}, "missing host"},
		{"embedded credentials", MCPServerConfig{URL: "https://user:pass@mcp.example.com/sse"}, "must not embed credentials"},
		{"empty url", MCPServerConfig{}, "url is required"},
		{"negative timeout", MCPServerConfig{URL: "https://mcp.example.com/sse", Timeout: -1}, "timeout must be non-negative"},
		{"empty required header", MCPServerConfig{URL: "https://mcp.example.com/sse", RequiredHeaders: []string{" "}}, "empty header name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MCPServers: map[string]MCPServerConfig{"s": tc.server}}
			errs := validateMCPServerConfigs(cfg)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("valid server rejected: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("invalid server accepted (want %q)", tc.want)
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("errors %v do not mention %q", errs, tc.want)
			}
		})
	}
}

// TestValidateAPIKeyToolGrants pins the NENYA-183 grant syntax check: a
// namespaced entry must name a configured server, and entries must be
// non-empty.
func TestValidateAPIKeyToolGrants(t *testing.T) {
	cfg := &Config{MCPServers: map[string]MCPServerConfig{"memory": {URL: "https://mcp.example.com/sse"}}}
	key := func(tools []string) *SecretsConfig {
		return &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
			"k": {Name: "k", Enabled: true, Roles: []string{"user"}, AllowedTools: tools},
		}}
	}

	if errs := validateAPIKeyToolGrants(cfg, key([]string{"memory__search", "memory__*", "search"})); len(errs) != 0 {
		t.Fatalf("valid grants rejected: %v", errs)
	}
	if errs := validateAPIKeyToolGrants(cfg, key([]string{"ghost__search"})); len(errs) != 1 || !strings.Contains(errs[0], "unknown mcp_servers[\"ghost\"]") {
		t.Fatalf("expected an unknown-server error, got %v", errs)
	}
	if errs := validateAPIKeyToolGrants(cfg, key([]string{" "})); len(errs) != 1 || !strings.Contains(errs[0], "empty tool name") {
		t.Fatalf("expected an empty-name error, got %v", errs)
	}
	if errs := validateAPIKeyToolGrants(cfg, key([]string{"__search"})); len(errs) != 1 || !strings.Contains(errs[0], "missing server prefix") {
		t.Fatalf("expected a missing-prefix error, got %v", errs)
	}
	// Disabled keys are inert.
	disabled := &SecretsConfig{ClientToken: "tok", ApiKeys: map[string]ApiKey{
		"k": {Name: "k", Enabled: false, AllowedTools: []string{"ghost__x"}},
	}}
	if errs := validateAPIKeyToolGrants(cfg, disabled); len(errs) != 0 {
		t.Fatalf("disabled keys must be skipped, got %v", errs)
	}
}

// TestValidateStreamStallTimeoutSeconds pins the NENYA-155 knob: nil = the
// 60s default, an explicit 0 disables, 86400 (24h) is the cap.
func TestValidateStreamStallTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name       string
		v          *int
		wantErr    bool
		wantErrMsg string
	}{
		{"nil means the default", nil, false, ""},
		{"explicit zero disables", PtrTo(0), false, ""},
		{"the 60s default is valid", PtrTo(60), false, ""},
		{"the 24h cap is valid", PtrTo(86400), false, ""},
		{"negative is invalid", PtrTo(-1), true, "must be non-negative"},
		{"past the cap is invalid", PtrTo(86401), true, "exceeds maximum allowed value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateStreamStallTimeoutSeconds(tt.v)
			if (len(errs) > 0) != tt.wantErr {
				t.Fatalf("validateStreamStallTimeoutSeconds(%v) errors = %v, wantErr %v", tt.v, errs, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(errs[0], tt.wantErrMsg) {
				t.Fatalf("error %q does not contain %q", errs[0], tt.wantErrMsg)
			}
		})
	}
}
