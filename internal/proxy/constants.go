package proxy

import "time"

const (
	mcpAutoSearchTimeout   = 10 * time.Second
	mcpLoopMaxDuration     = 5 * time.Minute
	mcpMaxIterations       = 10
	mcpMaxIterationsHardCeiling = 50
	maxEmbeddingsResponseBytes = 10 << 20
)