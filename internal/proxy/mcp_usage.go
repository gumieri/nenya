package proxy

import (
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

// mcpCalibrationModel returns the model key for the calibration tracker:
// the response-reported model when it matches one of the chain's targets
// (the key the serving dispatch estimate was recorded under — failover
// chains may serve a different target than [0]), else the primary target's
// model, else the raw response string. A mismatched key would split each
// estimate/actual pair across two models and silently pin the ratio at 1.0.
func mcpCalibrationModel(targets []routing.UpstreamTarget, buf *bufferedSSE) string {
	if buf != nil && buf.calibrationModel != "" {
		// Exact key: the serving target the dispatch estimate was
		// recorded under (failover chains may serve any target).
		return buf.calibrationModel
	}
	responseModel := ""
	if buf != nil {
		responseModel = buf.model
	}
	if responseModel != "" {
		for _, t := range targets {
			if t.Model == responseModel {
				return responseModel
			}
		}
	}
	if len(targets) > 0 && targets[0].Model != "" {
		return targets[0].Model
	}
	return responseModel
}

// recordCalibrationFromBuffer feeds the NENYA-135 tracker from a buffered
// response without touching Stats (per-iteration pairing; terminal paths
// own the Stats side via recordMCPUsage).
func recordCalibrationFromBuffer(gw *gateway.NenyaGateway, model string, buf *bufferedSSE) {
	if gw == nil || buf == nil || model == "" {
		return
	}
	chunk := buf.usageChunk()
	if chunk == nil {
		return
	}
	if usage, ok := chunk["usage"].(map[string]interface{}); ok {
		inputTokens, _, _ := extractTokenCounts(usage)
		gw.Calibration.RecordActual(model, inputTokens)
	}
}

func (p *Proxy) recordMCPUsage(gw *gateway.NenyaGateway, buf *bufferedSSE, agentName string) {
	if buf == nil || gw == nil || agentName == "" {
		return
	}
	chunk := buf.usageChunk()
	if chunk == nil {
		return
	}
	usage, ok := chunk["usage"].(map[string]interface{})
	if !ok {
		return
	}
	model := buf.model
	if model == "" {
		if m, ok := chunk["model"].(string); ok {
			model = m
		}
	}
	if model == "" {
		return
	}
	gw.Logger.Debug("MCP loop usage recorded",
		"agent", agentName, "model", model,
		"usage", usage)
	recordChatUsage(gw, model, usage)
}
