package main

import (
	"context"
	"flag"
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
	var configFile string
	var verbose bool
	var validateOnly bool
	flag.StringVar(&configFile, "config", "/etc/nenya/", "Path to configuration file or directory")
	flag.BoolVar(&verbose, "verbose", false, "Enable debug-level request/response logging")
	flag.BoolVar(&validateOnly, "validate", false, "Validate configuration and exit")
	flag.Parse()

	logger := infra.SetupLogger(verbose)

	cfg, err := config.Load(configFile)
	if err != nil {
		logger.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	secrets, err := config.LoadSecrets()
	if err != nil {
		logger.Error("failed to load secrets", "err", err)
		os.Exit(1)
	}

	if validateOnly {
		if err = config.ValidateConfiguration(cfg, secrets, logger); err != nil {
			logger.Error("configuration validation failed", "err", err)
			os.Exit(1)
		}
		logger.Info("configuration validation passed")
		os.Exit(0)
	}

	gw := gateway.New(*cfg, secrets, logger)
	p := &proxy.Proxy{}
	p.StoreGateway(gw)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	srv := &http.Server{
		Handler:        p,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 14,
	}

	listener, addr, err := systemdListener(cfg.Server.ListenAddr)
	if err != nil {
		logger.Error("failed to create listener", "err", err)
		os.Exit(1)
	}

	logger.Info("nenya ai gateway listening", "addr", addr, "socket_activation", listener != nil)

	if len(cfg.Agents) > 0 {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		logger.Info("agents configured", "agents", names)
	}

	if verbose {
		logger.Info("verbose logging enabled")
	}

	serverErr := make(chan error, 1)
	go func() {
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
	}()

	for {
		select {
		case err := <-serverErr:
			logger.Error("server failed", "err", err)
			os.Exit(1)
		case <-sighup:
			reloadConfig(p, configFile, logger)
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

func reloadConfig(p *proxy.Proxy, configFile string, logger *slog.Logger) {
	logger.Info("reloading configuration", "file", configFile)

	newCfg, err := config.Load(configFile)
	if err != nil {
		logger.Error("reload failed: could not load configuration file", "err", err)
		return
	}

	newSecrets, err := config.LoadSecrets()
	if err != nil {
		logger.Error("reload failed: could not load secrets", "err", err)
		return
	}

	if err := config.ValidateConfigurationNoPing(newCfg, newSecrets, logger); err != nil {
		logger.Error("reload failed: configuration validation", "err", err)
		return
	}

	oldGW := p.Gateway()
	newGW := oldGW.Reload(*newCfg, newSecrets)
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
	if err != nil {
		return nil, defaultAddr, err
	}

	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")

	addr := listener.Addr().String()
	return listener, addr, nil
}
