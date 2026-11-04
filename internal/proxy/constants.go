package proxy

import "time"

const (
	mcpAutoSearchTimeout       = 10 * time.Second
	maxEmbeddingsResponseBytes = 10 << 20
)
