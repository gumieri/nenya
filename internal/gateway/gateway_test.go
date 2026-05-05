package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"nenya/config"
	"nenya/internal/infra"
	"nenya/internal/security"
)

func testConfig() config.Config {
	return config.Config{
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{
			Enabled:       config.PtrTo(true),
			RedactPatterns: []string{`(?i)AKIA[0-9A-Z]{16}`, `sk-[a-zA-Z0-9]{48}`},
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
	cfg.Bouncer.Enabled = config.PtrTo(true)
	cfg.Bouncer.RedactPatterns = []string{`(?i)AKIA[0-9A-Z]{16}`, `sk-[a-zA-Z0-9]+`}
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
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(42)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(99999)
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
	newCfg.Governance.RatelimitMaxRPM = config.PtrTo(99)
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
	newCfg.Governance.RatelimitMaxRPM = config.PtrTo(42)
	newGW := gw.Reload(context.Background(), newCfg, testSecrets())

	if newGW.Config.Governance.RatelimitMaxRPM == nil || *newGW.Config.Governance.RatelimitMaxRPM != 42 {
		t.Fatalf("expected new RatelimitMaxRPM=42, got %v", newGW.Config.Governance.RatelimitMaxRPM)
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

func TestClose_ClearsSecureMem(t *testing.T) {
	cfg := testConfig()
	secrets := testSecrets()
	secrets.ClientToken = "test-client-token"
	secrets.ProviderKeys["test-provider"] = "test-provider-key"
	gw := New(context.Background(), cfg, secrets, testLogger())

	if gw.SecureMem == nil {
		t.Skip("secure memory not available, skipping")
	}

	if gw.ProviderKeyTokens == nil {
		t.Fatal("expected ProviderKeyTokens to be initialized")
	}
	if gw.ClientTokenRef == (security.SecureToken{}) {
		t.Fatal("expected ClientTokenRef to be initialized")
	}

	gw.Close()

	if gw.ProviderKeyTokens != nil {
		t.Error("expected ProviderKeyTokens to be cleared after Close")
	}
	if gw.ClientTokenRef != (security.SecureToken{}) {
		t.Error("expected ClientTokenRef to be cleared after Close")
	}
}

func TestMetrics_SecureMemFailures(t *testing.T) {
	cfg := testConfig()
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	if gw.Metrics == nil {
		t.Fatal("expected Metrics to be initialized")
	}
	gw.Metrics.RecordSecureMemInitFailure()
	gw.Metrics.RecordSecureMemSealFailure()
}

func TestProviderCanServe_WithAPIKey(t *testing.T) {
	provider := &config.Provider{APIKey: "test-key"}
	if !providerCanServe(provider) {
		t.Error("expected provider with API key to be servable")
	}
}

func TestProviderCanServe_WithAuthStyleNone(t *testing.T) {
	provider := &config.Provider{AuthStyle: "none"}
	if !providerCanServe(provider) {
		t.Error("expected provider with auth_style='none' to be servable")
	}
}

func TestProviderCanServe_WithoutAPIKeyOrNone(t *testing.T) {
	provider := &config.Provider{AuthStyle: "bearer"}
	if providerCanServe(provider) {
		t.Error("expected provider without API key and auth_style != 'none' to not be servable")
	}
}

func TestProviderCanServe_NilProvider(t *testing.T) {
	if providerCanServe(nil) {
		t.Error("expected nil provider to not be servable")
	}
}

func TestGetProvidersMap(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	providers := gw.GetProvidersMap()
	if providers == nil {
		t.Fatal("expected non-nil providers map")
	}
	if len(providers) == 0 {
		t.Error("expected providers map to be populated")
	}
}

func TestGetMCPClientsForAgent_AgentNotFound(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	clients := gw.GetMCPClientsForAgent("nonexistent-agent")
	if clients != nil {
		t.Error("expected nil clients for nonexistent agent")
	}
}

func TestGetMCPClientsForAgent_AgentHasNoMCP(t *testing.T) {
	cfg := testConfig()
	cfg.Agents = make(map[string]config.AgentConfig)
	cfg.Agents["test-agent"] = config.AgentConfig{}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	clients := gw.GetMCPClientsForAgent("test-agent")
	if clients != nil {
		t.Error("expected nil clients for agent without MCP config")
	}
}

func TestGetMCPClientsForAgent_AgentHasEmptyMCP(t *testing.T) {
	cfg := testConfig()
	cfg.Agents = make(map[string]config.AgentConfig)
	cfg.Agents["test-agent"] = config.AgentConfig{
		MCP: &config.AgentMCPConfig{Servers: []string{}},
	}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	clients := gw.GetMCPClientsForAgent("test-agent")
	if clients != nil {
		t.Error("expected nil clients for agent with empty MCP servers list")
	}
}

func TestGetMCPClientsForAgent_AgentWithMCP(t *testing.T) {
	cfg := testConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"test-server": {URL: "http://localhost:3000/sse"},
	}
	cfg.Agents = make(map[string]config.AgentConfig)
	cfg.Agents["test-agent"] = config.AgentConfig{
		MCP: &config.AgentMCPConfig{Servers: []string{"test-server"}},
	}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	clients := gw.GetMCPClientsForAgent("test-agent")
	if clients == nil {
		t.Fatal("expected non-nil clients map")
	}
	if len(clients) != 1 {
		t.Errorf("expected 1 client, got %d", len(clients))
	}
	if _, ok := clients["test-server"]; !ok {
		t.Error("expected test-server client to be present")
	}
}

func TestGetMCPClientsForAgent_AgentWithMultipleMCP(t *testing.T) {
	cfg := testConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"server-a": {URL: "http://localhost:3000/sse"},
		"server-b": {URL: "http://localhost:3001/sse"},
	}
	cfg.Agents = make(map[string]config.AgentConfig)
	cfg.Agents["test-agent"] = config.AgentConfig{
		MCP: &config.AgentMCPConfig{Servers: []string{"server-a", "server-b"}},
	}
	gw := New(context.Background(), cfg, testSecrets(), testLogger())

	clients := gw.GetMCPClientsForAgent("test-agent")
	if len(clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(clients))
	}
}

func TestGetProviderAPIKey_FromSecureMem(t *testing.T) {
	cfg := testConfig()
	secrets := testSecrets()
	secrets.ProviderKeys["test-provider"] = "secure-key"
	gw := New(context.Background(), cfg, secrets, testLogger())

	if gw.SecureMem == nil {
		t.Skip("secure memory not available, skipping")
	}

	key, ok := gw.GetProviderAPIKey("test-provider")
	if !ok {
		t.Error("expected to find provider key")
	}
	if string(key) != "secure-key" {
		t.Errorf("expected 'secure-key', got %q", string(key))
	}
}

func TestGetProviderAPIKey_FromConfigFallback(t *testing.T) {
	cfg := testConfig()
	secrets := &config.SecretsConfig{
		ClientToken:  "test",
		ProviderKeys: map[string]string{"test-provider": "config-key"},
	}
	gw := New(context.Background(), cfg, secrets, testLogger())

	key, ok := gw.GetProviderAPIKey("test-provider")
	if !ok {
		t.Error("expected to find provider key from config")
	}
	if string(key) != "config-key" {
		t.Errorf("expected 'config-key', got %q", string(key))
	}
}

func TestGetProviderAPIKey_NotFound(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	_, ok := gw.GetProviderAPIKey("nonexistent-provider")
	if ok {
		t.Error("expected not to find nonexistent provider key")
	}
}

func TestProviderHasAPIKey_FromSecureMem(t *testing.T) {
	cfg := testConfig()
	secrets := testSecrets()
	secrets.ProviderKeys["test-provider"] = "secure-key"
	gw := New(context.Background(), cfg, secrets, testLogger())

	if gw.SecureMem == nil {
		t.Skip("secure memory not available, skipping")
	}

	if !gw.ProviderHasAPIKey("test-provider") {
		t.Error("expected provider to have API key in secure mem")
	}
}
func TestProviderHasAPIKey_FromConfig(t *testing.T) {
	cfg := testConfig()
	secrets := &config.SecretsConfig{
		ClientToken:  "test",
		ProviderKeys: map[string]string{"test-provider": "config-key"},
	}
	gw := New(context.Background(), cfg, secrets, testLogger())

	if !gw.ProviderHasAPIKey("test-provider") {
		t.Error("expected provider to have API key from config")
}
}
func TestProviderHasAPIKey_AuthStyleNone(t *testing.T) {
	cfg := testConfig()
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {AuthStyle: "none"},
	}
	secrets := &config.SecretsConfig{ClientToken: "test"}
	gw := New(context.Background(), cfg, secrets, testLogger())

	if !gw.ProviderHasAPIKey("test-provider") {
		t.Error("expected provider with auth_style='none' to have API key")
	}
}
func TestProviderHasAPIKey_NotFound(t *testing.T) {
	gw := New(context.Background(), testConfig(), testSecrets(), testLogger())

	if gw.ProviderHasAPIKey("nonexistent-provider") {
		t.Error("expected nonexistent provider to not have API key")
	}
}

func TestExtractInputJSONFromPart_ValidJSON(t *testing.T) {
	part := map[string]interface{}{
		"input_json": map[string]interface{}{
			"key1": "value1",
			"key2": 42,
		},
	}
	result := extractInputJSONFromPart(part)
	if result == "" {
		t.Error("expected non-empty result for valid input_json")
	}
}

func TestExtractInputJSONFromPart_NilInput(t *testing.T) {
	part := map[string]interface{}{
		"input_json": nil,
	}
	result := extractInputJSONFromPart(part)
	if result != "" {
		t.Errorf("expected empty string for nil input_json, got %q", result)
	}
}

func TestExtractInputJSONFromPart_MarshalFailure(t *testing.T) {
	part := map[string]interface{}{
		"input_json": make(chan int),
	}
	result := extractInputJSONFromPart(part)
	if result != "" {
		t.Errorf("expected empty string on marshal failure, got %q", result)
	}
}

func TestExtractInputJSONFromPart_MissingField(t *testing.T) {
	part := map[string]interface{}{
		"type": "other",
	}
	result := extractInputJSONFromPart(part)
	if result != "" {
		t.Errorf("expected empty string for missing input_json, got %q", result)
	}
}


func TestContextWithTimeout(t *testing.T) {
	ctx := context.Background()
	timeout := 100 * time.Millisecond

	newCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()

	if newCtx == nil {
		t.Fatal("expected non-nil context")
	}

	select {
	case <-newCtx.Done():
		t.Error("context should not be cancelled immediately")
	default:
	}
}

func TestBuildMCPClients_EmptyConfig(t *testing.T) {
	cfg := testConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{}
	clients := buildMCPClients(cfg, testLogger())

	if clients == nil {
		t.Fatal("expected non-nil clients map")
	}
	if len(clients) != 0 {
		t.Errorf("expected 0 clients, got %d", len(clients))
	}
}

func TestBuildMCPClients_SkipsEmptyURL(t *testing.T) {
	cfg := testConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"empty-url": {URL: ""},
		"valid-url": {URL: "http://localhost:3000/sse"},
	}
	clients := buildMCPClients(cfg, testLogger())

	if len(clients) != 1 {
		t.Errorf("expected 1 client (empty URL skipped), got %d", len(clients))
	}
	if _, ok := clients["valid-url"]; !ok {
		t.Error("expected valid-url client to be present")
	}
	if _, ok := clients["empty-url"]; ok {
		t.Error("expected empty-url client to be skipped")
	}
}

func TestBuildMCPClients_MultipleServers(t *testing.T) {
	cfg := testConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"server-a": {URL: "http://localhost:3000/sse"},
		"server-b": {URL: "http://localhost:3001/sse", Timeout: 30, KeepAliveInterval: 60},
	}
	clients := buildMCPClients(cfg, testLogger())

	if len(clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(clients))
	}
}

func TestNewResponseCache_Disabled(t *testing.T) {
	cfg := testConfig()
	cfg.ResponseCache.Enabled = config.PtrTo(false)
	cache := newResponseCache(cfg, testLogger())

	if cache != nil {
		t.Error("expected nil cache when disabled")
	}
}

func TestNewResponseCache_Enabled(t *testing.T) {
	cfg := testConfig()
	cfg.ResponseCache = config.ResponseCacheConfig{
		Enabled:           config.PtrTo(true),
		MaxEntries:        100,
		MaxEntryBytes:     1024,
		TTLSeconds:        300,
		EvictEverySeconds: 60,
	}
	cache := newResponseCache(cfg, testLogger())

	if cache == nil {
		t.Fatal("expected non-nil cache when enabled")
	}
}

func TestCreateEntropyFilter_Disabled(t *testing.T) {
	cfg := testConfig()
	cfg.Bouncer.EntropyEnabled = false
	filter := createEntropyFilter(cfg, testLogger())

	if filter != nil {
		t.Error("expected nil filter when disabled")
	}
}

func TestCreateEntropyFilter_Enabled(t *testing.T) {
	cfg := testConfig()
	cfg.Bouncer.EntropyEnabled = true
	cfg.Bouncer.EntropyThreshold = 3.5
	cfg.Bouncer.EntropyMinToken = 10
	filter := createEntropyFilter(cfg, testLogger())

	if filter == nil {
		t.Fatal("expected non-nil filter when enabled")
	}
}

func TestInitSecureMem_NoSecrets(t *testing.T) {
	sm, ref := initSecureMem(nil, testLogger(), config.PtrTo(false), nil)

	if sm != nil {
		t.Error("expected nil secure mem without secrets")
	}
	if ref != (security.SecureToken{}) {
		t.Error("expected empty token ref without secrets")
	}
}

func TestInitSecureMem_EmptyClientToken(t *testing.T) {
	secrets := testSecrets()
	secrets.ClientToken = ""
	sm, ref := initSecureMem(secrets, testLogger(), config.PtrTo(false), nil)

	if sm != nil {
		t.Error("expected nil secure mem with empty client token")
	}
	if ref != (security.SecureToken{}) {
		t.Error("expected empty token ref with empty client token")
	}
}

func TestInitSecureMem_NotRequiredFallsBack(t *testing.T) {
	secrets := testSecrets()
	secrets.ClientToken = "test-token"

	var metrics *infra.Metrics
	sm, ref := initSecureMem(secrets, testLogger(), config.PtrTo(false), metrics)

	if ref == (security.SecureToken{}) && sm == nil {
		t.Log("secure mem unavailable (expected on some platforms)")
	}
}

func TestInitProviderKeyTokens_NoSecureMem(t *testing.T) {
	tokens := initProviderKeyTokens(nil, nil, testLogger(), nil)

	if tokens != nil {
		t.Error("expected nil tokens without secure mem")
	}
}

func TestInitProviderKeyTokens_NoProviderKeys(t *testing.T) {
	// Create a fresh secure mem for this test
	sm, err := security.NewSecureMem(security.TokenSizeHint(1, 0))
	if err != nil {
		t.Fatalf("failed to create secure mem: %v", err)
	}
	defer sm.Destroy()

	tokens := initProviderKeyTokens(sm, &config.SecretsConfig{}, testLogger(), nil)

	if tokens != nil {
		t.Error("expected nil tokens map when no provider keys")
	}
}

func TestInitProviderKeyTokens_SkipsEmptyKeys(t *testing.T) {
	// Create a fresh secure mem for this test
	sm, err := security.NewSecureMem(security.TokenSizeHint(0, 3)) // 3 provider keys max
	if err != nil {
		t.Fatalf("failed to create secure mem: %v", err)
	}
	defer sm.Destroy()

	secretCfg := &config.SecretsConfig{
		ProviderKeys: map[string]string{
			"valid-provider":   "valid-key",
			"empty-provider":   "",
			"another-provider": "another-key",
		},
	}

	tokens := initProviderKeyTokens(sm, secretCfg, testLogger(), nil)

	if len(tokens) != 2 {
		t.Errorf("expected 2 tokens (empty key skipped), got %d", len(tokens))
	}
	if _, ok := tokens["empty-provider"]; ok {
		t.Error("expected empty-provider to be skipped")
	}
}

func TestSealSecureMem_Nil(t *testing.T) {
	sealSecureMem(nil, testLogger(), nil)
}

func TestSealSecureMem_Valid(t *testing.T) {
	cfg := testConfig()
	secrets := testSecrets()
	gw := New(context.Background(), cfg, secrets, testLogger())

	if gw.SecureMem == nil {
		t.Skip("secure memory not available, skipping")
	}

	sealSecureMem(gw.SecureMem, testLogger(), nil)
}
