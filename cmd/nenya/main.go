package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sort"
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
	flag.StringVar(&configFile, "config", "config.json", "Path to configuration file")
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
		if err := config.ValidateConfiguration(cfg, secrets, logger); err != nil {
			logger.Error("configuration validation failed", "err", err)
			os.Exit(1)
		}
		logger.Info("configuration validation passed")
		os.Exit(0)
	}

	gw := gateway.New(*cfg, secrets, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:           cfg.Server.ListenAddr,
		Handler:        &proxy.Proxy{GW: gw},
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 14,
	}

	logger.Info("nenya ai gateway listening", "addr", cfg.Server.ListenAddr)

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
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		logger.Error("server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	logger.Info("shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}
