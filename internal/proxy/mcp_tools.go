package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	rawBytes     []byte
	toolCalls    []mcpToolCall
	finishReason string
	hasContent   bool
}

type mcpToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

func replayBufferedResponse(w http.ResponseWriter, buf *bufferedSSE) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if fw, ok := newImmediateFlushWriterSafe(w); ok {
		fw.Write(buf.rawBytes)
	} else {
		w.Write(buf.rawBytes)
	}
}

func bufferStreamResponse(ctx context.Context, r io.Reader) (*bufferedSSE, error) {
	var sb strings.Builder
	var toolCalls []mcpToolCall
	var finishReason string
	var hasContent bool

	// Accumulate tool call arguments across streaming chunks
	// keyed by tool call index
	tcArgsAccum := make(map[int]*strings.Builder)
	tcNameAccum := make(map[int]string)
	tcIDAccum := make(map[int]string)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

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

		sb.WriteString(data)
		sb.WriteString("\n\n")
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}

	// Build final tool calls from accumulated data
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

	return &bufferedSSE{
		rawBytes:     []byte(sb.String()),
		toolCalls:    toolCalls,
		finishReason: finishReason,
		hasContent:   hasContent,
	}, nil
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

			route, ok := gw.GW.MCPToolIndex.Lookup(c.Name)
			if !ok {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("unknown MCP tool: %s", c.Name)}},
					IsError: true,
				}
				return
			}

			client := gw.GW.MCPClients[route.ServerName]
			if client == nil {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP server not available: %s", route.ServerName)}},
					IsError: true,
				}
				return
			}

			toolCtx, cancel := context.WithTimeout(ctx, mcpExecTimeout)
			defer cancel()

			result, err := client.CallTool(toolCtx, route.MCPToolName, c.Arguments)
			if err != nil {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP tool call failed: %v", err)}},
					IsError: true,
				}
				return
			}
			results[idx] = result
		}(i, call)
	}

	wg.Wait()
	return results
}

func appendMCPResults(payload map[string]any, calls []mcpToolCall, results []*mcp.CallToolResult) {
	if len(calls) == 0 || len(results) == 0 {
		return
	}

	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}

	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "assistant" {
				if _, hasToolCalls := msg["tool_calls"]; hasToolCalls {
					lastAssistantIdx = i
					break
				}
			}
		}
	}

	if lastAssistantIdx == -1 {
		return
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
			"role":        "tool",
			"tool_call_id": call.ID,
			"content":     content,
		})
	}

	newMessages := make([]any, 0, len(messages)+len(toolResults))
	newMessages = append(newMessages, messages[:lastAssistantIdx+1]...)
	newMessages = append(newMessages, toolResults...)
	newMessages = append(newMessages, messages[lastAssistantIdx+1:]...)

	payload["messages"] = newMessages
}
