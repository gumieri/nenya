package config

import (
	"strings"
	"testing"
)

// selfLoopCfg builds a minimal config with the given listen address
// and one judgment entry whose engine resolves to a single target at
// the given URL.
func selfLoopCfg(listenAddr, targetURL string) *Config {
	cfg := &Config{}
	cfg.Server.ListenAddr = listenAddr
	cfg.Governance.Judgments = map[string]*JudgmentConfig{
		"probe": {Engine: &EngineRef{Provider: "stub", Model: "judge"}},
	}
	cfg.Governance.Judgments["probe"].Engine.ResolvedTargets = []EngineTarget{{
		Provider: &Provider{Name: "stub", URL: targetURL},
		Engine:   EngineConfig{Provider: "stub", Model: "judge"},
	}}
	return cfg
}

func TestValidateSelfLoopGuard(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		targetURL  string
		wantErr    bool
	}{
		{name: "loopback IP self-target rejected", listenAddr: ":8080", targetURL: "http://127.0.0.1:8080/v1/chat/completions", wantErr: true},
		{name: "localhost name self-target rejected", listenAddr: ":8080", targetURL: "http://localhost:8080/v1/chat/completions", wantErr: true},
		{name: "ipv6 loopback self-target rejected", listenAddr: "[::1]:8080", targetURL: "http://[::1]:8080/v1/chat/completions", wantErr: true},
		{name: "wildcard bind catches loopback target", listenAddr: "0.0.0.0:8080", targetURL: "http://127.0.0.1:8080/v1/chat/completions", wantErr: true},
		{name: "proxy path self-target rejected", listenAddr: ":8080", targetURL: "http://127.0.0.1:8080/proxy/openai", wantErr: true},
		{name: "same host different port allowed", listenAddr: ":8080", targetURL: "http://127.0.0.1:9090/v1/chat/completions", wantErr: false},
		{name: "remote provider allowed", listenAddr: ":8080", targetURL: "https://api.example.com/v1/chat/completions", wantErr: false},
		{name: "non-gateway path on same host+port allowed", listenAddr: ":8080", targetURL: "http://127.0.0.1:8080/other/service", wantErr: false},
		{name: "concrete listen host matches itself", listenAddr: "192.168.1.5:8080", targetURL: "http://192.168.1.5:8080/v1/chat/completions", wantErr: true},
		{name: "concrete listen host ignores other hosts", listenAddr: "192.168.1.5:8080", targetURL: "http://127.0.0.1:8080/v1/chat/completions", wantErr: false},
		{name: "unparseable listen addr stays silent", listenAddr: "not-an-addr", targetURL: "http://127.0.0.1:8080/v1/chat/completions", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateSelfLoopGuard(selfLoopCfg(tt.listenAddr, tt.targetURL))
			if tt.wantErr {
				if len(errs) != 1 {
					t.Fatalf("errs = %v, want 1 self-loop error", errs)
				}
				if !strings.Contains(errs[0], "self-proxying deadlock") || !strings.Contains(errs[0], "judgment_probe") {
					t.Errorf("error text incomplete: %q", errs[0])
				}
				return
			}
			if len(errs) != 0 {
				t.Errorf("unexpected errs: %v", errs)
			}
		})
	}
}

func TestValidateSelfLoopGuardCoversEngineSurfaces(t *testing.T) {
	cfg := selfLoopCfg(":8080", "https://api.example.com/v1")
	// Bouncer engine pointing at the gateway: must be flagged.
	cfg.Bouncer.Engine.ResolvedTargets = []EngineTarget{{
		Provider: &Provider{Name: "loop", URL: "http://127.0.0.1:8080/v1"},
		Engine:   EngineConfig{Provider: "loop", Model: "m"},
	}}
	// Window engine on a different port: must not be flagged.
	cfg.Window.Engine.ResolvedTargets = []EngineTarget{{
		Provider: &Provider{Name: "ok", URL: "http://127.0.0.1:9090/v1"},
		Engine:   EngineConfig{Provider: "ok", Model: "m"},
	}}
	// Enabled injection escalation pointing at the gateway: must be flagged.
	escEngine := &EngineRef{Provider: "loop", Model: "m"}
	escEngine.ResolvedTargets = []EngineTarget{{
		Provider: &Provider{Name: "loop", URL: "http://localhost:8080/v1/messages"},
		Engine:   EngineConfig{Provider: "loop", Model: "m"},
	}}
	cfg.Governance.Injection = &InjectionConfig{
		Enabled:    PtrTo(true),
		Escalation: &InjectionEscalationConfig{Enabled: PtrTo(true), Engine: escEngine},
	}

	errs := validateSelfLoopGuard(cfg)
	joined := strings.Join(errs, "\n")
	if strings.Count(joined, "self-proxying deadlock") != 2 {
		t.Fatalf("errs = %v, want exactly 2 self-loop errors (bouncer + escalation)", errs)
	}
	if !strings.Contains(joined, "bouncer_engine") {
		t.Error("bouncer_engine surface not flagged")
	}
	if !strings.Contains(joined, "injection_escalation_engine") {
		t.Error("injection_escalation_engine surface not flagged")
	}
	if strings.Contains(joined, "window_engine") || strings.Contains(joined, "judgment_probe") {
		t.Errorf("clean surfaces flagged: %q", joined)
	}
}

func TestSplitListenAddr(t *testing.T) {
	tests := []struct {
		addr string
		host string
		port string
		ok   bool
	}{
		{addr: ":8080", host: "", port: "8080", ok: true},
		{addr: "127.0.0.1:8080", host: "127.0.0.1", port: "8080", ok: true},
		{addr: "[::1]:8080", host: "::1", port: "8080", ok: true},
		{addr: "", ok: false},
		{addr: "garbage", ok: false},
	}
	for _, tt := range tests {
		host, port, ok := splitListenAddr(tt.addr)
		if ok != tt.ok || (ok && (host != tt.host || port != tt.port)) {
			t.Errorf("splitListenAddr(%q) = %q, %q, %v; want %q, %q, %v", tt.addr, host, port, ok, tt.host, tt.port, tt.ok)
		}
	}
}
