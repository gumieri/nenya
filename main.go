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
)

func main() {
	var configFile string
	var verbose bool
	flag.StringVar(&configFile, "config", "config.toml", "Path to configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable debug-level request/response logging")
	flag.Parse()

	logger := setupLogger(verbose)

	cfg, err := loadConfig(configFile)
	if err != nil {
		logger.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	secrets, err := loadSecrets()
	if err != nil {
		logger.Error("failed to load secrets", "err", err)
		os.Exit(1)
	}

	gateway := NewNenyaGateway(*cfg, secrets, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      gateway,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
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

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}
