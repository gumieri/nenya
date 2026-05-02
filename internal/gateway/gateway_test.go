package gateway

import (
	"context"
	"log/slog"
	"testing"

	"nenya/config"
)

func testConfig() config.Config {
	return config.Config{
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: 60,
			RatelimitMaxTPM: 100000,
		},
		SecurityFilter: config.SecurityFilterConfig{
			Enabled:  true,
			Patterns: []string{`(?i)AKIA[0-9A-Z]{16}`, `sk-[a-zA-Z0-9]{48}`},
		},
	}
}

func testSecrets() *config.SecretsConfig {
	return &config.SecretsConfig{
		ClientToken:  "test",
		ProviderKeys: map[string]string{},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNew_BuiltInProvidersMerged(t *testing.T) {
	cfg := testConfig()
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	if len(gw.Providers) == 0 {
		t.Fatal("expected built-in providers to be merged")
	}
	if _, ok := gw.Providers["zai"]; !ok {
		t.Error("expected zai provider to be present")
	}
}

func TestNew_SecretPatternsCompiled(t *testing.T) {
	cfg := testConfig()
	cfg.SecurityFilter.Enabled = true
	cfg.SecurityFilter.Patterns = []string{`(?i)AKIA[0-9A-Z]{16}`, `sk-[a-zA-Z0-9]+`}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	if len(gw.SecretPatterns) != 2 {
		t.Fatalf("expected 2 secret patterns, got %d", len(gw.SecretPatterns))
	}
	if !gw.SecretPatterns[0].MatchString("AKIAIOSFODNN7EXAMPLE") {
		t.Error("expected pattern to match AWS key")
	}
}

func TestNew_BlockedPatternsCompiled(t *testing.T) {
	cfg := testConfig()
	cfg.Governance.BlockedExecutionPatterns = []string{`(?i)\brm\s+-rf\b`, `(?i)\bshutdown\b`}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	if len(gw.BlockedPatterns) != 2 {
		t.Fatalf("expected 2 blocked patterns, got %d", len(gw.BlockedPatterns))
	}
	if !gw.BlockedPatterns[0].MatchString("rm -rf /") {
		t.Error("expected pattern to match rm -rf")
	}
}

func TestNew_AgentStateInitialized(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	if gw.AgentState == nil {
		t.Fatal("expected AgentState to be initialized")
	}
}

func TestNew_ThoughtSigCacheInitialized(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	if gw.ThoughtSigCache == nil {
		t.Fatal("expected ThoughtSigCache to be initialized")
	}
}

func TestNew_RateLimiterInitialized(t *testing.T) {
	cfg := testConfig()
	cfg.Governance.RatelimitMaxRPM = 42
	cfg.Governance.RatelimitMaxTPM = 99999
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	if gw.RateLimiter == nil {
		t.Fatal("expected RateLimiter to be initialized")
	}
}

func TestNew_StatsInitialized(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	if gw.Stats == nil {
		t.Fatal("expected Stats to be non-nil")
	}
}

func TestCountTokens_HelloWorld(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	tokens := gw.CountTokens("hello world")
	if tokens != 2 {
		t.Errorf("expected 2 tokens for 'hello world', got %d", tokens)
	}
}

func TestCountTokens_Contraction(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	tokens := gw.CountTokens("it's a test")
	if tokens != 4 {
		t.Errorf("expected 4 tokens for \"it's a test\", got %d", tokens)
	}
}

func TestCountTokens_EmptyString(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	tokens := gw.CountTokens("")
	if tokens != 0 {
		t.Errorf("expected 0 tokens for empty string, got %d", tokens)
	}
}

func TestCountTokens_Unicode(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	tokens := gw.CountTokens("こんにちは世界")
	if tokens != 4 {
		t.Errorf("expected 4 tokens for \"こんにちは世界\", got %d", tokens)
	}
}

func TestCountRequestTokens_NormalMessages(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"content": "hello"},
			map[string]interface{}{"content": "world"},
		},
	}

	tokens := gw.CountRequestTokens(payload)
	if tokens != 2 {
		t.Errorf("expected 2 tokens for 'helloworld' (10 chars / 4.0), got %d", tokens)
	}
}

func TestCountRequestTokens_EmptyPayload(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	tokens := gw.CountRequestTokens(map[string]interface{}{})
	if tokens != 0 {
		t.Errorf("expected 0 tokens for empty payload, got %d", tokens)
	}
}

func TestCountRequestTokens_MissingMessages(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	payload := map[string]interface{}{
		"model": "gpt-4",
	}

	tokens := gw.CountRequestTokens(payload)
	if tokens != 0 {
		t.Errorf("expected 0 tokens with missing messages field, got %d", tokens)
	}
}

func TestCountRequestTokens_ArrayContent(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello"},
					map[string]interface{}{"type": "text", "text": " world"},
				},
			},
		},
	}

	tokens := gw.CountRequestTokens(payload)
	if tokens != 2 {
		t.Errorf("expected 2 tokens for 'hello world' (11 chars / 4.0), got %d", tokens)
	}
}

func TestCountRequestTokens_NonMapMessagesSkipped(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	payload := map[string]interface{}{
		"messages": []interface{}{
			"not a map",
			42,
			map[string]interface{}{"content": "hello"},
		},
	}

	tokens := gw.CountRequestTokens(payload)
	if tokens != 1 {
		t.Errorf("expected 1 token for 'hello' (5 chars / 4.0), got %d", tokens)
	}
}

func TestExtractContentText_StringContent(t *testing.T) {
	msg := map[string]interface{}{"content": "hello world"}
	text := ExtractContentText(msg)
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
}

func TestExtractContentText_ArrayContent(t *testing.T) {
	msg := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "hello"},
			map[string]interface{}{"type": "text", "text": " world"},
			map[string]interface{}{"type": "image", "url": "http://example.com/img.png"},
		},
	}
	text := ExtractContentText(msg)
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
}

func TestExtractContentText_NilContent(t *testing.T) {
	msg := map[string]interface{}{"content": nil}
	text := ExtractContentText(msg)
	if text != "" {
		t.Errorf("expected empty string for nil content, got %q", text)
	}
}

func TestExtractContentText_MissingContent(t *testing.T) {
	msg := map[string]interface{}{"role": "user"}
	text := ExtractContentText(msg)
	if text != "" {
		t.Errorf("expected empty string for missing content, got %q", text)
	}
}

func TestExtractContentText_NonStringNonArrayContent(t *testing.T) {
	msg := map[string]interface{}{"content": 42}
	text := ExtractContentText(msg)
	if text != "" {
		t.Errorf("expected empty string for non-string non-array content, got %q", text)
	}
}

func TestReload_StatePreserved(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	stats := gw.Stats
	metrics := gw.Metrics
	sigCache := gw.ThoughtSigCache

	newCfg := testConfig()
	newCfg.Governance.RatelimitMaxRPM = 99
	newSecrets := testSecrets()
	newSecrets.ClientToken = "new-token"

	newGW := gw.Reload(context.Background(), newCfg, newSecrets)

	if newGW.Stats != stats {
		t.Fatal("expected Stats to be the same pointer")
	}
	if newGW.Metrics != metrics {
		t.Fatal("expected Metrics to be the same pointer")
	}
	if newGW.ThoughtSigCache != sigCache {
		t.Fatal("expected ThoughtSigCache to be the same pointer")
	}
}

func TestReload_ConfigUpdated(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	newCfg := testConfig()
	newCfg.Governance.RatelimitMaxRPM = 42
	newGW := gw.Reload(context.Background(), newCfg, testSecrets())

	if newGW.Config.Governance.RatelimitMaxRPM != 42 {
		t.Fatalf("expected new RatelimitMaxRPM=42, got %d", newGW.Config.Governance.RatelimitMaxRPM)
	}
}

func TestReload_ProvidersRebuilt(t *testing.T) {
	cfg := testConfig()
	secrets := testSecrets()
	secrets.ProviderKeys["gemini"] = "old-key"
	gw := New(context.Background(), cfg, secrets, testLogger())

	newSecrets := testSecrets()
	newSecrets.ProviderKeys["gemini"] = "new-key"
	newGW := gw.Reload(context.Background(), cfg, newSecrets)

	if newGW == gw {
		t.Fatal("expected new gateway pointer")
	}
	if newGW.Secrets != newSecrets {
		t.Fatal("expected new secrets reference")
	}
}
