package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/proxy"
)

func main() {
	configDir, configFile, verbose, validateOnly := parseFlags()

	logger := infra.SetupLogger(verbose)

	cfg, secrets, err := loadConfig(configDir, configFile, validateOnly, logger)
	if err != nil {
		logger.Error("setup failed", "err", err)
		os.Exit(1)
	}

	if validateOnly {
		validateCtx, validateCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer validateCancel()
		if err := config.ValidateConfiguration(validateCtx, cfg, secrets, logger); err != nil {
			logger.Error("configuration validation failed", "err", err)
			os.Exit(1)
		}
		logger.Info("configuration validation passed")
		return
	}

	run(logger, cfg, secrets, configDir, configFile)
}

func parseFlags() (configDir, configFile string, verbose, validateOnly bool) {
	flag.StringVar(&configDir, "config-dir", "", "Configuration directory (contains config.d/ or config.json)")
	flag.StringVar(&configFile, "config", "", "Single configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable debug-level request/response logging")
	flag.BoolVar(&validateOnly, "validate", false, "Validate configuration and exit")
	flag.Parse()

	if envConfigDir := os.Getenv("NENYA_CONFIG_DIR"); envConfigDir != "" {
		configDir = envConfigDir
	}
	if envConfigFile := os.Getenv("NENYA_CONFIG_FILE"); envConfigFile != "" {
		configFile = envConfigFile
	}

	if configDir == "" && configFile == "" {
		configDir = "/etc/nenya/"
	}

	return
}

func loadConfig(configDir, configFile string, validateOnly bool, logger *slog.Logger) (*config.Config, *config.SecretsConfig, error) {
	var cfg *config.Config
	var err error

	if configFile != "" {
		cfg, err = config.Load(configFile)
	} else {
		cfg, err = config.LoadFromDir(configDir)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}

	logger.Debug("configuration loaded",
		"discovery_enabled", cfg.Discovery.Enabled,
		"auto_agents", cfg.Discovery.AutoAgents,
		"auto_agents_config_provided", cfg.Discovery.AutoAgentsConfig != nil,
	)

	if listenAddr := os.Getenv("NENYA_LISTEN_ADDR"); listenAddr != "" {
		cfg.Server.ListenAddr = listenAddr
	} else if port := os.Getenv("PORT"); port != "" {
		cfg.Server.ListenAddr = ":" + port
	}

	secrets, err := config.LoadSecrets()
	if err != nil {
		return nil, nil, fmt.Errorf("load secrets: %w", err)
	}

	return cfg, secrets, nil
}

func run(logger *slog.Logger, cfg *config.Config, secrets *config.SecretsConfig, configDir, configFile string) {
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startupCancel()

	gw := gateway.New(startupCtx, *cfg, secrets, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	p := &proxy.Proxy{ShutdownCtx: ctx}
	p.StoreGateway(gw)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	srv := buildServer(p, cfg.Server.ListenAddr)

	listener, addr, err := systemdListener(cfg.Server.ListenAddr)
	if err != nil {
		logger.Error("failed to create listener", "err", err)
		os.Exit(1)
	}

	logger.Info("nenya ai gateway listening", "addr", addr, "socket_activation", listener != nil)
	logConfiguredAgents(logger, cfg)

	serverErr := make(chan error, 1)
	go serveHTTP(srv, listener, serverErr)

	eventLoop(logger, configDir, configFile, p, ctx, sighup, serverErr, srv)
}

func buildServer(p *proxy.Proxy, listenAddr string) *http.Server {
	return &http.Server{
		Handler:        p,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 14,
	}
}

func logConfiguredAgents(logger *slog.Logger, cfg *config.Config) {
	if len(cfg.Agents) == 0 {
		return
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	logger.Info("agents configured", "agents", names)
}

func serveHTTP(srv *http.Server, listener net.Listener, serverErr chan error) {
	if listener != nil {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	} else {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}
	close(serverErr)
}

func eventLoop(logger *slog.Logger, configDir, configFile string, p *proxy.Proxy, ctx context.Context, sighup chan os.Signal, serverErr chan error, srv *http.Server) {
	for {
		select {
		case err := <-serverErr:
			logger.Error("server failed", "err", err)
			os.Exit(1)
		case <-sighup:
			go func() {
				reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer reloadCancel()
				reloadConfig(reloadCtx, p, configDir, configFile, logger)
			}()
		case <-ctx.Done():
			logger.Info("shutting down gracefully...")

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error("graceful shutdown failed", "err", err)
			}
			logger.Info("server stopped")
			return
		}
	}
}

func reloadConfig(ctx context.Context, p *proxy.Proxy, configDir, configFile string, logger *slog.Logger) {
	logger.Info("reloading configuration", "config_dir", configDir, "config_file", configFile)

	var newCfg *config.Config
	var err error

	if configFile != "" {
		newCfg, err = config.Load(configFile)
	} else {
		newCfg, err = config.LoadFromDir(configDir)
	}
	if err != nil {
		logger.Error("reload failed: could not load configuration", "err", err)
		return
	}

	newSecrets, err := config.LoadSecrets()
	if err != nil {
		logger.Error("reload failed: could not load secrets", "err", err)
		return
	}

	if err := config.ValidateConfigurationNoPing(ctx, newCfg, newSecrets, logger); err != nil {
		logger.Error("reload failed: configuration validation", "err", err)
		return
	}

	oldGW := p.Gateway()
	newGW := oldGW.Reload(ctx, *newCfg, newSecrets)
	p.StoreGateway(newGW)

	logger.Info("configuration reloaded successfully")
}

const (
	sdListenFdsStart = 3
)

func systemdListener(defaultAddr string) (net.Listener, string, error) {
	listenPid := os.Getenv("LISTEN_PID")
	if listenPid == "" {
		return nil, defaultAddr, nil
	}

	pid, err := strconv.Atoi(listenPid)
	if err != nil {
		return nil, defaultAddr, nil
	}

	if pid != os.Getpid() {
		return nil, defaultAddr, nil
	}

	listenFds := os.Getenv("LISTEN_FDS")
	if listenFds == "" {
		return nil, defaultAddr, nil
	}

	nfds, err := strconv.Atoi(listenFds)
	if err != nil || nfds == 0 {
		return nil, defaultAddr, nil
	}

	fd := os.NewFile(uintptr(sdListenFdsStart), "systemd")
	listener, err := net.FileListener(fd)
	_ = fd.Close() // FileListener dups the fd; close the original in all cases
	if err != nil {
		return nil, defaultAddr, err
	}

	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")

	addr := listener.Addr().String()
	return listener, addr, nil
}
