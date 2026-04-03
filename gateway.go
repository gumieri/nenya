package main

import (
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

type NenyaGateway struct {
	config         Config
	client         *http.Client
	ollamaClient   *http.Client
	tokenizer      *tiktoken.Tiktoken
	secrets        *SecretsConfig
	providers      map[string]*Provider
	rateLimits     map[string]*rateLimiter
	secretPatterns []*regexp.Regexp
	stats          *UsageTracker
	logger         *slog.Logger
	rlMu           sync.Mutex
	agentCounters  map[string]uint64
	modelCooldowns map[string]time.Time
	agentMu        sync.Mutex
}

func NewNenyaGateway(cfg Config, secrets *SecretsConfig, logger *slog.Logger) *NenyaGateway {
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
		Timeout:   120 * time.Second,
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
		Timeout:   time.Duration(cfg.Ollama.TimeoutSeconds) * time.Second,
	}

	tokenizer, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		logger.Warn("failed to initialize cl100k_base tokenizer, falling back to rune-length counting", "err", err)
		tokenizer = nil
	}

	var secretPatterns []*regexp.Regexp
	if cfg.Filter.Enabled && len(cfg.Filter.Patterns) > 0 {
		for _, pattern := range cfg.Filter.Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warn("failed to compile secret pattern, skipping", "pattern", pattern, "err", err)
				continue
			}
			secretPatterns = append(secretPatterns, re)
		}
		logger.Info("compiled secret patterns for Tier-0 filtering", "count", len(secretPatterns))
	}

	return &NenyaGateway{
		config:         cfg,
		client:         secureClient,
		ollamaClient:   ollamaClient,
		tokenizer:      tokenizer,
		secrets:        secrets,
		providers:      resolveProviders(&cfg, secrets),
		rateLimits:     make(map[string]*rateLimiter),
		secretPatterns: secretPatterns,
		stats:          NewUsageTracker(),
		logger:         logger,
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
	}
}

func (g *NenyaGateway) countTokens(text string) int {
	if g.tokenizer != nil {
		tokens := g.tokenizer.Encode(text, nil, nil)
		return len(tokens)
	}
	return utf8.RuneCountInString(text) / 4
}

func extractContentText(msg map[string]interface{}) string {
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

func (g *NenyaGateway) countRequestTokens(payload map[string]interface{}) int {
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
		sb.WriteString(extractContentText(msg))
	}
	return g.countTokens(sb.String())
}
