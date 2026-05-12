package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/proxy"
	"nenya/internal/version"
)

type configPaths struct {
	dir  string
	file string
}

type reloadLimiter struct {
	mu         sync.Mutex
	pending    bool
	debounce   *time.Timer
	debounceMu sync.Mutex
}

func (rl *reloadLimiter) Stop() {
	rl.debounceMu.Lock()
	defer rl.debounceMu.Unlock()
	if rl.debounce != nil {
		rl.debounce.Stop()
		rl.debounce = nil
	}
}

func (rl *reloadLimiter) tryStart() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.pending {
		return false
	}
	rl.pending = true
	return true
}

func (rl *reloadLimiter) done() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.pending = false
}

func (rl *reloadLimiter) scheduleReload(reloadFunc func()) bool {
	rl.debounceMu.Lock()
	defer rl.debounceMu.Unlock()

	if rl.debounce != nil {
		rl.debounce.Stop()
	}

	rl.debounce = time.AfterFunc(200*time.Millisecond, func() {
		rl.debounceMu.Lock()
		rl.debounce = nil
		rl.debounceMu.Unlock()

		if rl.tryStart() {
			defer rl.done()
			reloadFunc()
		}
	})

	return true
}

const (
	sdListenFdsStart = 3
)

func main() {
	paths, verbose, validateOnly, printSchema := parseFlags()

	if printSchema {
		schema, err := config.PrintSchema()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error generating schema: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(schema)
		return
	}

	cfg, secrets, err := loadConfig(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := setupLoggerFromConfig(cfg, verbose)
	logger.Info("starting nenya", "version", version.Version, "commit", version.Commit, "build_time", version.BuildTime)

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

	run(logger, cfg, secrets, paths)
}

func parseFlags() (configPaths, bool, bool, bool) {
	return parseArgs(os.Args[1:])
}

func parseArgs(args []string) (configPaths, bool, bool, bool) {
	fs := flag.NewFlagSet("nenya", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var configDir, configFile string
	var verbose, validateOnly, printSchema bool

	fs.StringVar(&configDir, "config-dir", "", "Configuration directory (contains config.d/ or config.json)")
	fs.StringVar(&configFile, "config", "", "Single configuration file")
	fs.BoolVar(&verbose, "verbose", false, "Enable debug-level request/response logging")
	fs.BoolVar(&validateOnly, "validate", false, "Validate configuration and exit")
	fs.BoolVar(&printSchema, "print-config-schema", false, "Print JSON Schema of config and exit")
	_ = fs.Parse(args)

	paths := effectiveConfigPaths(configDir, configFile)
	return paths, verbose, validateOnly, printSchema
}

func effectiveConfigPaths(configDir, configFile string) configPaths {
	dir, file := configDir, configFile

	if envConfigDir := os.Getenv("NENYA_CONFIG_DIR"); envConfigDir != "" {
		dir = envConfigDir
	}
	if envConfigFile := os.Getenv("NENYA_CONFIG_FILE"); envConfigFile != "" {
		file = envConfigFile
	}

	if dir == "" && file == "" {
		dir = "/etc/nenya/"
	}

	return configPaths{dir: dir, file: file}
}

func loadConfig(paths configPaths) (*config.Config, *config.SecretsConfig, error) {
	var cfg *config.Config
	var err error

	if paths.file != "" {
		cfg, err = config.Load(paths.file)
	} else {
		cfg, err = config.LoadFromDir(paths.dir)
	}
	if err != nil {
		return nil, nil, err
	}

	secrets, err := config.LoadSecrets()
	if err != nil {
		return nil, nil, err
	}
	return cfg, secrets, nil
}

func setupLoggerFromConfig(cfg *config.Config, verbose bool) *slog.Logger {
	if verbose {
		return infra.SetupLogger(true)
	}

	level := config.LogLevelFromString(cfg.Server.LogLevel)
	if err := infra.SetLogLevel(cfg.Server.LogLevel); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set log level: %v\n", err)
	}
	return infra.SetupLoggerWithLevel(level)
}

func run(logger *slog.Logger, cfg *config.Config, secrets *config.SecretsConfig, paths configPaths) {
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

	eventLoop(logger, paths, p, ctx, sighup, serverErr, srv)
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

func eventLoop(logger *slog.Logger, paths configPaths, p *proxy.Proxy, ctx context.Context, sighup chan os.Signal, serverErr chan error, srv *http.Server) {
	var rl reloadLimiter

	for {
		select {
		case err := <-serverErr:
			logger.Error("server failed", "err", err)
			os.Exit(1)
		case <-sighup:
			logger.Debug("received SIGHUP, scheduling debounced reload")
			rl.scheduleReload(func() {
				reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer reloadCancel()
				reloadConfig(reloadCtx, p, paths, logger)
			})
		case <-ctx.Done():
			logger.Info("shutting down gracefully...")

			rl.Stop()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := p.Shutdown(shutdownCtx); err != nil {
				logger.Error("gateway shutdown failed", "err", err)
			}

			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error("HTTP server shutdown failed", "err", err)
			}
			logger.Info("server stopped")
			return
		}
	}
}

func reloadConfig(ctx context.Context, p *proxy.Proxy, paths configPaths, logger *slog.Logger) {
	logger.Info("reloading configuration", "config_dir", paths.dir, "config_file", paths.file)

	var newCfg *config.Config
	var err error

	if paths.file != "" {
		newCfg, err = config.Load(paths.file)
	} else {
		newCfg, err = config.LoadFromDir(paths.dir)
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
	_ = fd.Close()
	if err != nil {
		return nil, defaultAddr, err
	}

	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")

	addr := listener.Addr().String()
	return listener, addr, nil
}
