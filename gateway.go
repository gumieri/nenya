package main

import (
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// NenyaGateway encapsulates the proxy state and HTTP client.
type NenyaGateway struct {
	config         Config
	client         *http.Client // upstream API client (120s timeout)
	ollamaClient   *http.Client // local Ollama client (longer timeout for inference)
	tokenizer      *tiktoken.Tiktoken
	secrets        *SecretsConfig
	rateLimits     map[string]*rateLimiter // keyed by upstream host
	secretPatterns []*regexp.Regexp
	verbose        bool       // enable debug-level request/response logging
	rlMu           sync.Mutex
	agentCounters  map[string]uint64    // round-robin counter per agent name
	modelCooldowns map[string]time.Time // cooldown expiry keyed by "provider:model"
	agentMu        sync.Mutex           // protects agentCounters and modelCooldowns
}

// NewNenyaGateway initializes the proxy with strict security settings.
func NewNenyaGateway(cfg Config, secrets *SecretsConfig) *NenyaGateway {
	// Security: Custom HTTP client with strict timeouts to prevent connection hanging.
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
		Timeout:   120 * time.Second, // Total request timeout
	}

	// Ollama runs locally and inference can take much longer than 120s for
	// large models. Use a separate transport without ResponseHeaderTimeout —
	// the shared transport's 30s header timeout would fire before Ollama even
	// starts streaming tokens for large models, making the client Timeout
	// effectively unreachable.
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
		log.Printf("[WARN] Failed to initialize cl100k_base tokenizer: %v. Falling back to byte length counting.", err)
		tokenizer = nil
	}

	// Compile secret patterns if filter is enabled
	var secretPatterns []*regexp.Regexp
	if cfg.Filter.Enabled && len(cfg.Filter.Patterns) > 0 {
		for _, pattern := range cfg.Filter.Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				log.Printf("[WARN] Failed to compile secret pattern %q: %v. Skipping.", pattern, err)
				continue
			}
			secretPatterns = append(secretPatterns, re)
		}
		log.Printf("[INFO] Compiled %d secret patterns for Tier‑0 filtering", len(secretPatterns))
	}

	return &NenyaGateway{
		config:         cfg,
		client:         secureClient,
		ollamaClient:   ollamaClient,
		tokenizer:      tokenizer,
		secrets:        secrets,
		rateLimits:     make(map[string]*rateLimiter),
		secretPatterns: secretPatterns,
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
	}
}

// countTokens returns the number of tokens in text using cl100k_base if available,
// otherwise falls back to rune-length approximation (runes / 4).
func (g *NenyaGateway) countTokens(text string) int {
	if g.tokenizer != nil {
		tokens := g.tokenizer.Encode(text, nil, nil)
		return len(tokens)
	}
	// Fallback: rough approximation for English text (4 runes ≈ 1 token)
	// This is inaccurate for non-English text but better than nothing
	return utf8.RuneCountInString(text) / 4
}

// countRequestTokens counts tokens in the message content of a parsed OpenAI-format
// request body, ignoring JSON structure overhead. Handles both string and multi-part
// ([]{"type":"text","text":"..."}) content formats.
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
		switch content := msg["content"].(type) {
		case string:
			sb.WriteString(content)
		case []interface{}:
			for _, partRaw := range content {
				if part, ok := partRaw.(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
		}
	}
	return g.countTokens(sb.String())
}
