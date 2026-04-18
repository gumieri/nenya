package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"nenya/internal/mcp"
)

const (
	mcpMaxIterations = 10
	mcpExecTimeout   = 30 * time.Second
)

type bufferedSSE struct {
	rawBytes         []byte
	toolCalls        []mcpToolCall
	assistantMessage map[string]any
	finishReason     string
	hasContent       bool
	model            string
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if fw, ok := newImmediateFlushWriterSafe(w); ok {
		fw.Write(buf.rawBytes)
	} else {
		w.Write(buf.rawBytes)
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
		fw.Write([]byte(sseData))
	} else {
		w.Write([]byte(sseData))
	}
}

func bufferStreamResponse(ctx context.Context, r io.Reader) (*bufferedSSE, error) {
	var sb strings.Builder
	var toolCalls []mcpToolCall
	var finishReason string
	var hasContent bool
	var contentBuilder strings.Builder
	var model string

	tcArgsAccum := make(map[int]*strings.Builder)
	tcNameAccum := make(map[int]string)
	tcIDAccum := make(map[int]string)
	totalLines := 0
	dataLines := 0

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}
		totalLines++

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataLines++

		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}

		if data == "[DONE]" {
			sb.WriteString("data: [DONE]\n\n")
			break
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if m, ok := chunk["model"].(string); ok && m != "" {
			model = m
		}

		var delta map[string]any
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if d, ok := choice["delta"].(map[string]any); ok {
					delta = d
				}
			}
		}

		if delta != nil {
			if content, ok := delta["content"]; ok && content != nil {
				if s, ok := content.(string); ok && s != "" {
					hasContent = true
					contentBuilder.WriteString(s)
				}
			}
			if tcSlice, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcSlice {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if idxVal, ok := tc["index"].(float64); ok {
						idx = int(idxVal)
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						tcIDAccum[idx] = id
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok && name != "" {
							tcNameAccum[idx] = name
						}
						if argsAny, ok := fn["arguments"]; ok {
							if argsStr, ok := argsAny.(string); ok && argsStr != "" {
								if tcArgsAccum[idx] == nil {
									tcArgsAccum[idx] = &strings.Builder{}
								}
								tcArgsAccum[idx].WriteString(argsStr)
							}
						}
					}
				}
			}
		}

		if fr, ok := chunk["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		} else if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					finishReason = fr
				}
			}
		}

		sb.WriteString("data: ")
		sb.WriteString(data)
		sb.WriteString("\n\n")
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}

	// Detect non-SSE responses (e.g. plain JSON)
	if dataLines == 0 && totalLines > 0 {
		slog.Warn("MCP stream: no SSE 'data:' lines found; possible non-SSE response",
			"total_lines", totalLines)
	}

	for idx := 0; idx < len(tcIDAccum); idx++ {
		id := tcIDAccum[idx]
		name := tcNameAccum[idx]
		if name == "" || id == "" {
			continue
		}
		var args map[string]any
		if argsBuilder := tcArgsAccum[idx]; argsBuilder != nil {
			argsStr := argsBuilder.String()
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				args = make(map[string]any)
			}
		}
		toolCalls = append(toolCalls, mcpToolCall{
			ID:        id,
			Name:      name,
			Arguments: args,
		})
	}

	var assistantMsg map[string]any
	if len(toolCalls) > 0 {
		assistantMsg = map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": buildOpenAIToolCalls(toolCalls),
		}
	} else if hasContent {
		assistantMsg = map[string]any{
			"role":    "assistant",
			"content": contentBuilder.String(),
		}
	}

	return &bufferedSSE{
		rawBytes:         []byte(sb.String()),
		toolCalls:        toolCalls,
		assistantMessage: assistantMsg,
		finishReason:     finishReason,
		hasContent:       hasContent,
		model:            model,
	}, nil
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

func executeMCPCalls(ctx context.Context, calls []mcpToolCall, gw *Proxy) []*mcp.CallToolResult {
	if len(calls) == 0 {
		return nil
	}

	results := make([]*mcp.CallToolResult, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c mcpToolCall) {
			defer wg.Done()

				if gw.Gateway() == nil {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "gateway not available"}},
					IsError: true,
				}
				return
			}

			logger := gw.Gateway().Logger
			if logger == nil {
				logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			}

			start := time.Now()

			route, ok := gw.Gateway().MCPToolIndex.Lookup(c.Name)
			if !ok {
				logger.Warn("MCP tool call failed: unknown tool", "tool", c.Name)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("unknown MCP tool: %s", c.Name)}},
					IsError: true,
				}
				if gw.Gateway() != nil && gw.Gateway().Metrics != nil {
					gw.Gateway().Metrics.RecordMCPToolCall("unknown", c.Name, "", time.Since(start), fmt.Errorf("unknown tool"))
				}
				return
			}

			client := gw.Gateway().MCPClients[route.ServerName]
			if client == nil {
				logger.Warn("MCP tool call failed: server not available", "server", route.ServerName, "tool", c.Name)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP server not available: %s", route.ServerName)}},
					IsError: true,
				}
				if gw.Gateway() != nil && gw.Gateway().Metrics != nil {
					gw.Gateway().Metrics.RecordMCPToolCall(route.ServerName, c.Name, "", time.Since(start), fmt.Errorf("server not available"))
				}
				return
			}

			toolCtx, cancel := context.WithTimeout(ctx, mcpExecTimeout)
			defer cancel()

			result, err := client.CallTool(toolCtx, route.MCPToolName, c.Arguments)
			duration := time.Since(start)
			if err != nil {
				logger.Warn("MCP tool call failed", "server", route.ServerName, "tool", route.MCPToolName, "err", err, "duration_ms", duration.Milliseconds())
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP tool call failed: %v", err)}},
					IsError: true,
				}
			} else {
				results[idx] = result
				if gw.Gateway() != nil && gw.Gateway().Metrics != nil {
					textLen := 0
					if result != nil {
						textLen = len(result.Text())
					}
						logger.Debug("MCP tool call completed", "server", route.ServerName, "tool", route.MCPToolName, "duration_ms", duration.Milliseconds(), "result_bytes", textLen)
				}

				if gw.Gateway() != nil && gw.Gateway().Metrics != nil {
					gw.Gateway().Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, "", duration, err)
				}
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

	newMessages := make([]any, 0, len(messages)+len(toolResults))
	newMessages = append(newMessages, messages...)
	newMessages = append(newMessages, toolResults...)

	payload["messages"] = newMessages
}
