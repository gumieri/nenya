package main

import (
	"flag"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

func main() {
	var configFile string
	var verbose bool
	flag.StringVar(&configFile, "config", "config.toml", "Path to configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable debug-level request/response logging")
	flag.Parse()

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("[FATAL] Failed to load configuration: %v", err)
	}

	secrets, err := loadSecrets()
	if err != nil {
		log.Fatalf("[FATAL] Failed to load secrets: %v", err)
	}

	gateway := NewNenyaGateway(*cfg, secrets)
	gateway.verbose = verbose

	// Security: Strict server timeouts.
	// WriteTimeout is 0 (disabled) because this is a streaming proxy — the upstream
	// client timeout (secureClient.Timeout = 120s) and Ollama client timeout bound
	// the actual response duration. A non-zero WriteTimeout would cut off slow
	// streaming responses before the upstream finishes.
	srv := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      gateway,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("[INFO] Nenya AI Gateway listening on %s", cfg.Server.ListenAddr)
	log.Printf("[INFO] Providers: gemini, deepseek, zai, groq, together, ollama")
	if len(cfg.Agents) > 0 {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		log.Printf("[INFO] Agents: %s", strings.Join(names, ", "))
	}
	if verbose {
		log.Printf("[INFO] Verbose logging enabled")
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[FATAL] Server failed: %v", err)
	}
}
