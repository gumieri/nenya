package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/proxy"
	"nenya/internal/testutil"
)

func TestParseFlags_Defaults(t *testing.T) {
	paths, verbose, validateOnly, printSchema := parseFlags()
	if paths.dir != "/etc/nenya/" {
		t.Errorf("expected /etc/nenya/, got %s", paths.dir)
	}
	if paths.file != "" {
		t.Errorf("expected empty config file, got %s", paths.file)
	}
	if verbose {
		t.Error("expected verbose=false")
	}
	if validateOnly {
		t.Error("expected validateOnly=false")
	}
	if printSchema {
		t.Error("expected printSchema=false")
	}
}

func TestParseFlags_EnvConfigDir(t *testing.T) {
	t.Setenv("NENYA_CONFIG_DIR", "/custom/config")
	paths, _, _, _ := parseFlags()
	if paths.dir != "/custom/config" {
		t.Errorf("expected /custom/config, got %s", paths.dir)
	}
	if paths.file != "" {
		t.Errorf("expected empty config file, got %s", paths.file)
	}
}

func TestParseFlags_EnvConfigFile(t *testing.T) {
	t.Setenv("NENYA_CONFIG_FILE", "/custom/config.json")
	paths, _, _, _ := parseFlags()
	if paths.file != "/custom/config.json" {
		t.Errorf("expected /custom/config.json, got %s", paths.file)
	}
}

func TestParseFlags_ConfigDirTakesPrecedence(t *testing.T) {
	t.Setenv("NENYA_CONFIG_DIR", "/env/config")
	paths, _, _, _ := parseFlags()
	if paths.dir != "/env/config" {
		t.Errorf("expected /env/config, got %s", paths.dir)
	}
}

func TestBuildServer(t *testing.T) {
	p := &proxy.Proxy{}
	srv := buildServer(p, ":9090")
	if srv.Handler != p {
		t.Error("server handler should be the proxy")
	}
	if srv.Addr != ":9090" {
		t.Errorf("expected Addr :9090, got %s", srv.Addr)
	}
	if srv.ReadTimeout != 10*time.Second {
		t.Errorf("expected ReadTimeout 10s, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("expected WriteTimeout 0, got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("expected IdleTimeout 120s, got %v", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<14 {
		t.Errorf("expected MaxHeaderBytes %d, got %d", 1<<14, srv.MaxHeaderBytes)
	}
}

func TestLogConfiguredAgents_Empty(t *testing.T) {
	cfg := testutil.MinimalConfig()
	logger := testutil.NewTestLogger()
	logConfiguredAgents(logger, cfg)
}

func TestLogConfiguredAgents_WithAgents(t *testing.T) {
	cfg := testutil.TestConfig(testutil.WithAgent("test-agent", config.AgentConfig{
		Strategy: "fallback",
		Models: []config.AgentModel{
			{Provider: "openai", Model: "gpt-4", MaxContext: 128000, MaxOutput: 4096},
		},
	}))
	logger := testutil.NewTestLogger()
	logConfiguredAgents(logger, cfg)
}

func TestLoadConfig_FromFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server": {"listen_addr": ":9090"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(`{"client_token": "test-token-12345"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NENYA_SECRETS_DIR", secretsDir)

	paths := configPaths{file: configPath}
	cfg, secrets, err := loadConfig(paths)
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
	if secrets == nil || secrets.ClientToken != "test-token-12345" {
		t.Errorf("expected client_token test-token-12345, got %v", secrets)
	}
}


func TestLoadConfig_FromDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config.d")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "00-server.json"), []byte(`{"server": {"listen_addr": ":7070"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(`{"client_token": "test-token-12345"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NENYA_SECRETS_DIR", secretsDir)

	paths := configPaths{dir: dir}
	cfg, secrets, err := loadConfig(paths)
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if cfg.Server.ListenAddr != ":7070" {
		t.Errorf("expected :7070, got %s", cfg.Server.ListenAddr)
	}
	if secrets == nil || secrets.ClientToken != "test-token-12345" {
		t.Errorf("expected client_token test-token-12345, got %v", secrets)
	}
}

func TestLoadConfig_ConfigFail(t *testing.T) {
	paths := configPaths{dir: "/nonexistent"}
	_, _, err := loadConfig(paths)
	if err == nil {
		t.Fatal("expected error for nonexistent config")
	}
}

func TestApplyListenAddrFromEnv_PORTOnly(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("HOST", "")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_PORTAndHOST(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("HOST", "127.0.0.1")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_NoEnv(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("HOST", "")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080 (unchanged), got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_InvalidPort(t *testing.T) {
	t.Setenv("PORT", "not-a-number")
	t.Setenv("HOST", "")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080 (unchanged after invalid PORT), got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_HostOnly(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("HOST", "127.0.0.1")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080 (HOST alone ignored), got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_EmptyConfigAddr(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("HOST", "")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ""}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_PortZero(t *testing.T) {
	t.Setenv("PORT", "0")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":0" {
		t.Errorf("expected :0 (random port), got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_PortMax(t *testing.T) {
	t.Setenv("PORT", "65535")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":65535" {
		t.Errorf("expected :65535, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_PortOutOfRange(t *testing.T) {
	t.Setenv("PORT", "99999")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080 unchanged for out-of-range port, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_IPv6Host(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("HOST", "::1")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != "[::1]:9090" {
		t.Errorf("expected [::1]:9090, got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_Whitespace(t *testing.T) {
	t.Setenv("PORT", "  9090  ")
	t.Setenv("HOST", "  127.0.0.1  ")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090 (trimmed whitespace), got %s", cfg.Server.ListenAddr)
	}
}

func TestApplyListenAddrFromEnv_NegativePort(t *testing.T) {
	t.Setenv("PORT", "-1")
	cfg := &config.Config{Server: config.ServerConfig{ListenAddr: ":8080"}}
	applyListenAddrFromEnv(cfg, slog.Default())
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("expected :8080 unchanged for negative port, got %s", cfg.Server.ListenAddr)
	}
}

func TestSystemdListener_NoEnv(t *testing.T) {
	listener, addr, err := systemdListener(":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listener != nil {
		t.Error("expected nil listener without LISTEN_PID")
	}
	if addr != ":8080" {
		t.Errorf("expected :8080, got %s", addr)
	}
}

func TestSystemdListener_InvalidPid(t *testing.T) {
	t.Setenv("LISTEN_PID", "not-a-number")
	listener, addr, err := systemdListener(":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listener != nil {
		t.Error("expected nil listener for invalid PID")
	}
	if addr != ":8080" {
		t.Errorf("expected :8080, got %s", addr)
	}
}

func TestSystemdListener_WrongPid(t *testing.T) {
	t.Setenv("LISTEN_PID", "999999")
	t.Setenv("LISTEN_FDS", "1")
	listener, addr, err := systemdListener(":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listener != nil {
		t.Error("expected nil listener for wrong PID")
	}
	if addr != ":8080" {
		t.Errorf("expected :8080, got %s", addr)
	}
}

func TestSystemdListener_NoFds(t *testing.T) {
	t.Setenv("LISTEN_PID", fmt.Sprintf("%d", os.Getpid()))
	listener, addr, err := systemdListener(":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listener != nil {
		t.Error("expected nil listener without LISTEN_FDS")
	}
	if addr != ":8080" {
		t.Errorf("expected :8080, got %s", addr)
	}
}

func TestServeHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	srv := &http.Server{}
	serverErr := make(chan error, 1)
	go serveHTTP(srv, listener, serverErr)

	srv.Close()
	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for serveHTTP to finish")
	}
}

func TestServeHTTP_NilListener(t *testing.T) {
	srv := &http.Server{}
	serverErr := make(chan error, 1)

	done := make(chan struct{})
	go func() {
		serveHTTP(srv, nil, serverErr)
		close(done)
	}()

	srv.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		srv.Close()
		t.Fatal("timeout waiting for serveHTTP to finish")
	}
}

func TestEventLoop_Shutdown(t *testing.T) {
	logger := testutil.NewTestLogger()
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test-token-1234567890"}, logger)
	p := &proxy.Proxy{}
	p.StoreGateway(gw)

	ctx, cancel := context.WithCancel(context.Background())
	sighup := make(chan os.Signal, 1)
	serverErr := make(chan error, 1)
	srv := buildServer(p, ":0")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	go serveHTTP(srv, listener, serverErr)

	done := make(chan struct{})
	go func() {
		eventLoop(logger, configPaths{}, p, ctx, sighup, serverErr, srv)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for eventLoop shutdown")
	}
}

func TestReloadConfig_Success(t *testing.T) {
	logger := testutil.NewTestLogger()
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test-token-1234567890"}, logger)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server": {"listen_addr": ":9090"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(`{"client_token": "test-token-1234567890"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NENYA_SECRETS_DIR", secretsDir)

	p := &proxy.Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	reloadConfig(ctx, p, configPaths{file: configPath}, logger)

	newGW := p.Gateway()
	if newGW == nil {
		t.Fatal("gateway should not be nil after reload")
	}
}

func TestReloadConfig_ConfigFail(t *testing.T) {
	logger := testutil.NewTestLogger()
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test-token-1234567890"}, logger)

	p := &proxy.Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	reloadConfig(ctx, p, configPaths{file: "/nonexistent/config.json"}, logger)

	if p.Gateway() != gw {
		t.Error("gateway should remain unchanged after failed reload")
	}
}

func TestServeHTTP_ServerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	listener.Close()

	srv := &http.Server{}
	serverErr := make(chan error, 1)

	go serveHTTP(srv, listener, serverErr)

	select {
	case err := <-serverErr:
		if err == nil {
			t.Error("expected an error for closed listener")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for serveHTTP error")
	}
}

func TestReloadLimiter_TryStart(t *testing.T) {
	var rl reloadLimiter

	if !rl.tryStart() {
		t.Error("expected tryStart to return true on first call")
	}
	if rl.tryStart() {
		t.Error("expected tryStart to return false while pending")
	}

	rl.done()
	if !rl.tryStart() {
		t.Error("expected tryStart to return true after done")
	}
}

func TestReloadLimiter_ConcurrentAccess(t *testing.T) {
	var rl reloadLimiter
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rl.done()
			_ = rl.tryStart()
		}()
	}
	wg.Wait()
}

func TestEventLoop_ConcurrentSighup(t *testing.T) {
	logger := testutil.NewTestLogger()
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test"}, logger)

	p := &proxy.Proxy{}
	p.StoreGateway(gw)

	ctx, cancel := context.WithCancel(context.Background())
	sighup := make(chan os.Signal, 10)
	serverErr := make(chan error, 1)
	srv := buildServer(p, ":0")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	go serveHTTP(srv, listener, serverErr)

	done := make(chan struct{})
	go func() {
		eventLoop(logger, configPaths{}, p, ctx, sighup, serverErr, srv)
		close(done)
	}()

	for i := 0; i < 5; i++ {
		sighup <- syscall.SIGHUP
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for eventLoop shutdown")
	}
}
