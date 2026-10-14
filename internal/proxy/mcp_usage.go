package proxy

import (
	"encoding/json"
	"strings"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
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

// usageTotals returns the per-dimension high-water merge of every usage object
// in the buffer (NENYA-167). An Anthropic-native stream reports the input side
// on message_start and the output side on message_delta, so taking the last
// usage-bearing chunk alone would lose the input (and the calibration actual);
// for the OpenAI shape, whose usage object is cumulative, the merge is a no-op.
func (b *bufferedSSE) usageTotals() (stream.UsageTotals, bool) {
	if b.usageMergedParsed {
		return b.usageMerged, b.usageMergedSeen
	}
	b.usageMergedParsed = true
	for _, line := range strings.Split(string(b.rawBytes), "\n") {
		line = strings.TrimPrefix(line, "data: ")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" || !strings.Contains(line, "usage") {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		usage, ok := stream.UsageMapFromChunk(chunk)
		if !ok {
			continue
		}
		b.usageMerged = mergeUsageTotals(b.usageMerged, stream.NormalizeUsageTotals(usage))
		b.usageMergedSeen = true
	}
	return b.usageMerged, b.usageMergedSeen
}

// mergeUsageTotals keeps the per-dimension high-water mark, mirroring the
// streaming reader's cumulative accounting so no dimension is counted twice.
func mergeUsageTotals(acc, next stream.UsageTotals) stream.UsageTotals {
	acc.Prompt = max(acc.Prompt, next.Prompt)
	acc.Completion = max(acc.Completion, next.Completion)
	acc.CacheHit = max(acc.CacheHit, next.CacheHit)
	acc.CacheMiss = max(acc.CacheMiss, next.CacheMiss)
	acc.CacheCreation = max(acc.CacheCreation, next.CacheCreation)
	acc.Reasoning = max(acc.Reasoning, next.Reasoning)
	acc.Total = max(acc.Total, next.Total)
	acc.TotalReported = acc.TotalReported || next.TotalReported
	if !acc.TotalReported {
		acc.Total = acc.Prompt + acc.Completion
	}
	return acc
}

// recordCalibrationFromBuffer feeds the NENYA-135 tracker from a buffered
// response without touching Stats (per-iteration pairing; terminal paths
// own the Stats side via recordMCPUsage).
func recordCalibrationFromBuffer(gw *gateway.NenyaGateway, model string, buf *bufferedSSE) {
	if gw == nil || buf == nil || model == "" {
		return
	}
	totals, ok := buf.usageTotals()
	if !ok {
		return
	}
	gw.Calibration.RecordActual(model, totals.Prompt)
}

func (p *Proxy) recordMCPUsage(gw *gateway.NenyaGateway, buf *bufferedSSE, agentName string) {
	if buf == nil || gw == nil || agentName == "" {
		return
	}
	chunk := buf.usageChunk()
	if chunk == nil {
		return
	}
	totals, ok := buf.usageTotals()
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
		"usage", totals)
	recordChatUsageTotals(gw, model, totals)
}
