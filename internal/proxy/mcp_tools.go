package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"nenya/internal/gateway"
	"nenya/internal/mcp"
	"nenya/internal/util"
)

const (
	mcpExecTimeout = 30 * time.Second
)

type bufferedSSE struct {
	rawBytes         []byte
	toolCalls        []mcpToolCall
	assistantMessage map[string]any
	finishReason     string
	hasContent       bool
	model            string
	reasoningContent string
}

type mcpToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

func replayBufferedResponse(w http.ResponseWriter, buf *bufferedSSE, logger *slog.Logger) {
	if len(buf.rawBytes) == 0 {
		logger.Warn("MCP loop: empty buffered response, sending error")
		writeSSEError(w, http.StatusInternalServerError, "MCP loop: empty response from upstream")
		return
	}

	logger.Info("replaying buffered SSE response",
		"has_content", buf.hasContent,
		"finish_reason", buf.finishReason,
		"raw_bytes_len", len(buf.rawBytes))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if fw, ok := newImmediateFlushWriterSafe(w); ok {
		if _, err := fw.Write(buf.rawBytes); err != nil {
			return
		}
	} else {
		if _, err := w.Write(buf.rawBytes); err != nil {
			return
		}
	}
}

func writeSSEError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(statusCode)

	errPayload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "gateway_error",
		},
	}
	errBytes, _ := json.Marshal(errPayload)
	sseData := fmt.Sprintf("data: %s\n\n", errBytes)

	if fw, ok := newImmediateFlushWriterSafe(w); ok {
		if _, err := fw.Write([]byte(sseData)); err != nil {
			return
		}
	} else {
		if _, err := w.Write([]byte(sseData)); err != nil {
			return
		}
	}
}

// sseAccumulator accumulates SSE stream state during buffered reading,
// decomposing the high-cyclomatic-complexity bufferStreamResponse into
// single-responsibility methods.
type sseAccumulator struct {
	raw          strings.Builder
	finishReason string
	hasContent   bool
	content      strings.Builder
	reasoning    strings.Builder
	model        string
	logger       *slog.Logger

	tcArgsAccum map[int]*strings.Builder
	tcNameAccum map[int]string
	tcIDAccum   map[int]string
	totalLines  int
	dataLines   int
}

func newSSEAccumulator(logger *slog.Logger) *sseAccumulator {
	return &sseAccumulator{
		tcArgsAccum: make(map[int]*strings.Builder),
		tcNameAccum: make(map[int]string),
		tcIDAccum:   make(map[int]string),
		logger:      logger,
	}
}

// processLine handles a single scanner line. Returns true when [DONE] is seen.
func (acc *sseAccumulator) processLine(line string) bool {
	if line == "" {
		return false
	}
	acc.totalLines++

	if !strings.HasPrefix(line, "data: ") {
		return false
	}
	acc.dataLines++

	data := strings.TrimPrefix(line, "data: ")
	data = strings.TrimSpace(data)
	if data == "" {
		return false
	}

	if data == "[DONE]" {
		acc.raw.WriteString("data: [DONE]\n\n")
		return true
	}

	acc.processChunk(data)
	acc.raw.WriteString("data: ")
	acc.raw.WriteString(data)
	acc.raw.WriteString("\n\n")
	return false
}

// processChunk unmarshals a JSON data chunk and extracts model, delta, and
// finish_reason.
func (acc *sseAccumulator) processChunk(data string) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}

	if m, ok := chunk["model"].(string); ok && m != "" {
		acc.model = m
	}

	if choice := extractFirstChoice(chunk); choice != nil {
		if d, ok := choice["delta"].(map[string]any); ok {
			acc.processDelta(d)
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			acc.finishReason = fr
		}
	}

	if acc.finishReason == "" {
		if fr, ok := chunk["finish_reason"].(string); ok && fr != "" {
			acc.finishReason = fr
		}
	}
}

// extractFirstChoice extracts the first choices[0] element from a chunk.
func extractFirstChoice(chunk map[string]any) map[string]any {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	return choice
}

// processDelta extracts content, reasoning_content, and tool_calls from a
// delta map.
func (acc *sseAccumulator) processDelta(delta map[string]any) {
	if content, ok := delta["content"]; ok && content != nil {
		if s, ok := content.(string); ok && s != "" {
			acc.hasContent = true
			acc.content.WriteString(s)
		}
	}

	if rc, ok := delta["reasoning_content"]; ok && rc != nil {
		if s, ok := rc.(string); ok && s != "" {
			acc.reasoning.WriteString(s)
		}
	}

	if tcSlice, ok := delta["tool_calls"].([]any); ok {
		for _, tcAny := range tcSlice {
			tc, ok := tcAny.(map[string]any)
			if !ok {
				continue
			}
			acc.processToolCallChunk(tc)
		}
	}
}

// processToolCallChunk extracts index, id, name, and arguments from a single
// tool_call delta chunk, accumulating arguments across chunks by index.
func (acc *sseAccumulator) processToolCallChunk(tc map[string]any) {
	idx := 0
	if idxVal, ok := tc["index"].(float64); ok {
		idx = int(idxVal)
	}

	if id, ok := tc["id"].(string); ok && id != "" {
		acc.tcIDAccum[idx] = id
	}

	fn, ok := tc["function"].(map[string]any)
	if !ok {
		return
	}

	if name, ok := fn["name"].(string); ok && name != "" {
		acc.tcNameAccum[idx] = name
	}

	if argsAny, ok := fn["arguments"]; ok {
		argsStr, ok := argsAny.(string)
		if !ok || argsStr == "" {
			return
		}
		if acc.tcArgsAccum[idx] == nil {
			acc.tcArgsAccum[idx] = &strings.Builder{}
		}
		acc.tcArgsAccum[idx].WriteString(argsStr)
	}
}

// buildToolCalls assembles the final tool call list from accumulators.
func (acc *sseAccumulator) buildToolCalls() []mcpToolCall {
	indices := make([]int, 0, len(acc.tcNameAccum))
	for idx := range acc.tcNameAccum {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	calls := make([]mcpToolCall, 0, len(indices))
	for _, idx := range indices {
		name := acc.tcNameAccum[idx]
		if name == "" {
			continue
		}
		id := acc.tcIDAccum[idx]
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		var args map[string]any
		if argsBuilder := acc.tcArgsAccum[idx]; argsBuilder != nil {
			argsStr := argsBuilder.String()
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				acc.logger.Warn("MCP: failed to parse tool arguments, calling with empty args",
					"tool", name, "err", err)
				args = make(map[string]any)
			}
		}
		calls = append(calls, mcpToolCall{ID: id, Name: name, Arguments: args})
	}
	return calls
}

// result produces the final bufferedSSE from accumulated state.
func (acc *sseAccumulator) result() *bufferedSSE {
	if acc.dataLines == 0 && acc.totalLines > 0 {
		acc.logger.Warn("MCP stream: no SSE 'data:' lines found; possible non-SSE response",
			"total_lines", acc.totalLines)
	}

	toolCalls := acc.buildToolCalls()
	reasoningStr := acc.reasoning.String()

	var assistantMsg map[string]any
	if len(toolCalls) > 0 {
		assistantMsg = map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": buildOpenAIToolCalls(toolCalls),
		}
		if reasoningStr != "" {
			assistantMsg["reasoning_content"] = reasoningStr
		}
	} else if acc.hasContent {
		assistantMsg = map[string]any{
			"role":    "assistant",
			"content": acc.content.String(),
		}
		if reasoningStr != "" {
			assistantMsg["reasoning_content"] = reasoningStr
		}
	}

	return &bufferedSSE{
		rawBytes:         []byte(acc.raw.String()),
		toolCalls:        toolCalls,
		assistantMessage: assistantMsg,
		finishReason:     acc.finishReason,
		hasContent:       acc.hasContent,
		model:            acc.model,
		reasoningContent: reasoningStr,
	}
}

func bufferStreamResponse(ctx context.Context, r io.Reader, logger *slog.Logger) (*bufferedSSE, error) {
	acc := newSSEAccumulator(logger)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if acc.processLine(scanner.Text()) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}
	return acc.result(), nil
}

func buildOpenAIToolCalls(calls []mcpToolCall) []any {
	result := make([]any, 0, len(calls))
	for _, call := range calls {
		argsBytes, _ := json.Marshal(call.Arguments)
		result = append(result, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(argsBytes),
			},
		})
	}
	return result
}

func partitionMCPToolCalls(calls []mcpToolCall, toolIndex *mcp.ToolRegistry) (mcpCalls, nonMcpCalls []mcpToolCall) {
	for _, call := range calls {
		if toolIndex.IsMCPTool(call.Name) {
			mcpCalls = append(mcpCalls, call)
		} else {
			nonMcpCalls = append(nonMcpCalls, call)
		}
	}
	return
}

func executeMCPCalls(ctx context.Context, calls []mcpToolCall, gw *gateway.NenyaGateway, agentName string) []*mcp.CallToolResult {
	if len(calls) == 0 {
		return nil
	}

	results := make([]*mcp.CallToolResult, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c mcpToolCall) {
			defer wg.Done()

			logger := gw.Logger
			if logger == nil {
				logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			}

			ctxLogger := logger.With(
				"mcp_operation", "tool_call",
				"agent", agentName,
				"mcp_call_id", c.ID,
			)

			start := time.Now()

			route, ok := gw.MCPToolIndex.Lookup(c.Name)
			if !ok {
				ctxLogger.Warn("MCP tool call failed: unknown tool", "tool", c.Name)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("unknown MCP tool: %s", c.Name)}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall("unknown", c.Name, agentName, time.Since(start), fmt.Errorf("unknown tool"))
				return
			}

			ctxLogger = ctxLogger.With(
				"mcp_server", route.ServerName,
				"mcp_tool", route.MCPToolName,
			)

			client := gw.MCPClients[route.ServerName]
			if client == nil {
				ctxLogger.Warn("MCP tool call failed: server not available")
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP server not available: %s", route.ServerName)}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall(route.ServerName, c.Name, agentName, time.Since(start), fmt.Errorf("server not available"))
				return
			}

			toolCtx, cancel := context.WithTimeout(ctx, mcpExecTimeout)
			defer cancel()

			result, err := client.CallTool(toolCtx, route.MCPToolName, c.Arguments)
			duration := time.Since(start)
			if err != nil {
				ctxLogger.Warn("MCP tool call failed",
					"err", err,
					"duration_ms", duration.Milliseconds())
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP tool call failed: %v", err)}},
					IsError: true,
				}
			} else {
				results[idx] = result
				gw.Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, agentName, duration, err)
				textLen := 0
				if result != nil {
					textLen = len(result.Text())
				}
				ctxLogger.Debug("MCP tool call completed",
					"duration_ms", duration.Milliseconds(),
					"result_bytes", textLen)
			}
		}(i, call)
	}

	wg.Wait()
	return results
}

func appendMCPResults(payload map[string]any, calls []mcpToolCall, results []*mcp.CallToolResult, assistantMsg map[string]any) {
	if len(calls) == 0 || len(results) == 0 {
		return
	}

	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}

	// Inject assistant message if provided
	if assistantMsg != nil {
		messages = append(messages, assistantMsg)
	}

	var toolResults []any
	for i, call := range calls {
		if i >= len(results) || results[i] == nil {
			continue
		}
		result := results[i]
		content := result.Text()
		if result.IsError {
			content = fmt.Sprintf("[MCP Error] %s", content)
		}

		toolResults = append(toolResults, map[string]any{
			"role":         "tool",
			"tool_call_id": call.ID,
			"content":      content,
		})
	}

	newMessages := make([]any, 0, util.AddCap(len(messages), len(toolResults)))
	newMessages = append(newMessages, messages...)
	newMessages = append(newMessages, toolResults...)

	payload["messages"] = newMessages
}
