package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestApplyDefaults_Bouncer(t *testing.T) {
	cfg := &Config{}
	if err := ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Bouncer.Enabled {
		t.Error("bouncer should be auto-enabled with defaults")
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		t.Error("bouncer should have default redact patterns")
	}
	if cfg.Bouncer.RedactionLabel != "[REDACTED]" {
		t.Errorf("expected [REDACTED], got %s", cfg.Bouncer.RedactionLabel)
	}
	if cfg.Bouncer.RedactOutputWindow != 4096 {
		t.Errorf("expected 4096, got %d", cfg.Bouncer.RedactOutputWindow)
	}
	if !cfg.Bouncer.FailOpen {
		t.Error("fail_open should default to true")
	}
}

func TestApplyDefaults_Bouncer_RespectsExplicitFailOpen(t *testing.T) {
	raw := `{"bouncer": {"fail_open": false}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Bouncer.FailOpen {
		t.Error("bouncer.fail_open should be false when explicitly set")
	}
}

func TestApplyDefaults_Bouncer_WithRedactPreset(t *testing.T) {
	raw := `{"bouncer": {"redact_preset": "credentials"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Bouncer.Enabled {
		t.Error("bouncer should be enabled when redact_preset is set")
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		t.Error("bouncer should have patterns from preset")
	}
}

func TestApplyDefaults_Governance(t *testing.T) {
	cfg := &Config{}
	if err := ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.TruncationKeepFirstPct != 15.0 {
		t.Errorf("expected 15.0, got %f", cfg.Governance.TruncationKeepFirstPct)
	}
	if cfg.Governance.TruncationKeepLastPct != 25.0 {
		t.Errorf("expected 25.0, got %f", cfg.Governance.TruncationKeepLastPct)
	}
}

func TestApplyDefaults_Window(t *testing.T) {
	cfg := &Config{}
	if err := ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Window.MaxContext != 128000 {
		t.Errorf("expected 128000, got %d", cfg.Window.MaxContext)
	}
}

func TestApplyDefaults_Server(t *testing.T) {
	cfg := &Config{}
	if err := ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080, got %s", cfg.Server.ListenAddr)
	}
	if cfg.Server.UserAgent == "" {
		t.Error("user agent should have a default")
	}
}

func TestStripComments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no comments",
			input: `{"key": "value"}`,
			want:  `{"key": "value"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(StripComments([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadFromDir_ConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server": {"listen_addr": ":9090"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
}

func TestLoadFromDir_ConfigDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config.d")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "00-server.json"), []byte(`{"server": {"listen_addr": ":9090"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "01-bouncer.json"), []byte(`{"bouncer": {"redact_preset": "pii"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		t.Error("expected pii patterns to be expanded")
	}
}

func TestLoadFromDir_MultiConfigMerge(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config.d")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "00-server.json"), []byte(`{"server": {"listen_addr": ":9090", "log_level": "debug"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "01-override.json"), []byte(`{"server": {"log_level": "info"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("expected info (overridden), got %s", cfg.Server.LogLevel)
	}
}

func TestLoadFromDir_MissingConfig(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFromDir(dir)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{invalid`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEngineRef_StringShorthand(t *testing.T) {
	raw := `"my-agent"`
	var ref EngineRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.AgentName != "my-agent" {
		t.Errorf("expected my-agent, got %s", ref.AgentName)
	}
}

func TestEngineRef_ProviderModelShorthand(t *testing.T) {
	raw := `"ollama/qwen2.5-coder:7b"`
	var ref EngineRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "ollama" {
		t.Errorf("expected ollama, got %s", ref.Provider)
	}
	if ref.Model != "qwen2.5-coder:7b" {
		t.Errorf("expected qwen2.5-coder:7b, got %s", ref.Model)
	}
}

func TestEngineRef_ObjectForm(t *testing.T) {
	raw := `{"provider": "ollama", "model": "qwen2.5-coder:7b", "timeout_seconds": 120}`
	var ref EngineRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "ollama" {
		t.Errorf("expected ollama, got %s", ref.Provider)
	}
	if ref.TimeoutSeconds != 120 {
		t.Errorf("expected 120, got %d", ref.TimeoutSeconds)
	}
}

func TestBouncerConfig_UnmarshalJSON_RespectsEnabled(t *testing.T) {
	raw := `{"bouncer": {"enabled": false, "redact_preset": "credentials"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Bouncer.Enabled {
		t.Error("bouncer should remain disabled when explicitly set to false")
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		t.Error("patterns should still be loaded even when disabled")
	}
}

func TestBouncerConfig_UnmarshalJSON_FailOpenTracking(t *testing.T) {
	raw := `{"fail_open": false}`
	var b BouncerConfig
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	if !b.FailOpenWasSet() {
		t.Error("failOpenSet should be true after explicit setting")
	}
	if b.FailOpen {
		t.Error("fail_open should be false")
	}
}

func TestEngineRef_NoDefaultOllama(t *testing.T) {
	var ref EngineRef
	raw := `{}`
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "" {
		t.Errorf("expected empty provider, got %s", ref.Provider)
	}
	if ref.Model != "" {
		t.Errorf("expected empty model, got %s", ref.Model)
	}
}
