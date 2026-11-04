package mcpproxy

import "time"

// Loop bounds moved with the MCP subsystem (Phase 040). mcpAutoSearchTimeout
// stays in the parent: its only use is the chat path's auto-search dispatch.
const (
	// mcpLoopMaxDuration bounds one full multi-turn tool loop.
	mcpLoopMaxDuration = 5 * time.Minute
	// mcpMaxIterations is the default tool-loop iteration cap.
	mcpMaxIterations = 10
	// mcpMaxIterationsHardCeiling caps any agent-configured iteration count.
	mcpMaxIterationsHardCeiling = 50
)
