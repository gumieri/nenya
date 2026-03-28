package main

import (
	"testing"
	"time"
)

// newTestGateway builds a minimal NenyaGateway suitable for unit tests.
func newTestGateway(cfg Config) *NenyaGateway {
	return &NenyaGateway{
		config:         cfg,
		secrets:        &SecretsConfig{},
		rateLimits:     make(map[string]*rateLimiter),
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
	}
}

func TestCountTokens(t *testing.T) {
	cfg := Config{
		Upstream: UpstreamConfig{},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	text := "Hello, world! This is a test."
	tokens := g.countTokens(text)
	// Token count is approximate; just ensure it's positive
	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}
