package gateway

import (
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"nenya/internal/config"
	"nenya/internal/infra"
	"nenya/internal/routing"
)

type NenyaGateway struct {
	Config          config.Config
	Client          *http.Client
	OllamaClient    *http.Client
	Secrets         *config.SecretsConfig
	Providers       map[string]*config.Provider
	RateLimiter     *infra.RateLimiter
	SecretPatterns  []*regexp.Regexp
	BlockedPatterns []*regexp.Regexp
	Stats           *infra.UsageTracker
	Metrics         *infra.Metrics
	Logger          *slog.Logger
	AgentState      *routing.AgentState
	ThoughtSigCache *infra.ThoughtSignatureCache
}

func New(cfg config.Config, secrets *config.SecretsConfig, logger *slog.Logger) *NenyaGateway {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]config.ProviderConfig)
	}
	for name, builtIn := range config.BuiltInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

	secureClient := &http.Client{
		Transport: transport,
	}

	ollamaTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
	}
	ollamaClient := &http.Client{
		Transport: ollamaTransport,
	}

	providers := config.ResolveProviders(&cfg, secrets)

	var secretPatterns []*regexp.Regexp
	if cfg.SecurityFilter.Enabled && len(cfg.SecurityFilter.Patterns) > 0 {
		for _, pattern := range cfg.SecurityFilter.Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warn("failed to compile secret pattern, skipping", "pattern", pattern, "err", err)
				continue
			}
			secretPatterns = append(secretPatterns, re)
		}
		logger.Info("compiled secret patterns for Tier-0 filtering", "count", len(secretPatterns))
	}

	var blockedPatterns []*regexp.Regexp
	for _, pattern := range cfg.Governance.BlockedExecutionPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			logger.Warn("failed to compile blocked execution pattern, skipping", "pattern", pattern, "err", err)
			continue
		}
		blockedPatterns = append(blockedPatterns, re)
	}
	if len(blockedPatterns) > 0 {
		logger.Info("compiled blocked execution patterns for stream kill switch", "count", len(blockedPatterns))
	}

	gw := &NenyaGateway{
		Config:          cfg,
		Client:          secureClient,
		OllamaClient:    ollamaClient,
		Secrets:         secrets,
		Providers:       providers,
		RateLimiter:     infra.NewRateLimiter(cfg.Governance.RatelimitMaxRPM, cfg.Governance.RatelimitMaxTPM),
		SecretPatterns:  secretPatterns,
		BlockedPatterns: blockedPatterns,
		Stats:           infra.NewUsageTracker(),
		Metrics:         nil,
		Logger:          logger,
		AgentState:      routing.NewAgentState(),
		ThoughtSigCache: infra.NewThoughtSignatureCache(1000, 30*time.Minute),
	}

	gw.Metrics = infra.NewMetrics()
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot
	gw.Metrics.Cooldowns = gw.AgentState.ActiveCooldowns
	gw.Metrics.CBStates = gw.AgentState.CBSnapshot

	return gw
}

func (g *NenyaGateway) InitMetrics() {
	g.Metrics = infra.NewMetrics()
	g.Metrics.RateLimits = g.RateLimiter.Snapshot
	g.Metrics.Cooldowns = g.AgentState.ActiveCooldowns
	g.Metrics.CBStates = g.AgentState.CBSnapshot
}

func (g *NenyaGateway) CountTokens(text string) int {
	ratio := g.Config.Server.TokenRatio
	if ratio <= 0 {
		ratio = 4.0
	}
	return int(float64(utf8.RuneCountInString(text)) / ratio)
}

func (g *NenyaGateway) CountRequestTokens(payload map[string]interface{}) int {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return 0
	}
	var sb strings.Builder
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		sb.WriteString(ExtractContentText(msg))
	}
	return g.CountTokens(sb.String())
}

func ExtractContentText(msg map[string]interface{}) string {
	contentRaw, ok := msg["content"]
	if !ok {
		return ""
	}
	switch content := contentRaw.(type) {
	case string:
		return content
	case []interface{}:
		var sb strings.Builder
		for _, partRaw := range content {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if text, ok := part["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}
