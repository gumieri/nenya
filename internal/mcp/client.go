package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

const ClientVersion = "0.1.0"

type Client struct {
	transport *HTTPTransport
	name      string
	logger    *slog.Logger

	mu          sync.RWMutex
	tools       []Tool
	toolsMap    map[string]Tool
	initialized bool
	serverInfo  ImplementationInfo
}

type ClientConfig struct {
	Name              string
	URL               string
	Headers           map[string]string
	ConnectTimeout    time.Duration
	RequestTimeout    time.Duration
	IdleTimeout       time.Duration
	KeepAliveInterval time.Duration
	Logger            *slog.Logger
}

func NewClient(cfg ClientConfig) *Client {
	name := cfg.Name
	if name == "" {
		name = "nenya"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	transport := NewHTTPTransport(TransportConfig{
		URL:               cfg.URL,
		Headers:           cfg.Headers,
		ConnectTimeout:    cfg.ConnectTimeout,
		RequestTimeout:    cfg.RequestTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		KeepAliveInterval: cfg.KeepAliveInterval,
		Logger:            logger,
	})

	return &Client{
		transport: transport,
		name:      name,
		logger:    logger,
		toolsMap:  make(map[string]Tool),
	}
}

func (c *Client) Initialize(ctx context.Context) error {
	if err := c.transport.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	params := InitializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    ClientCapabilities{},
		ClientInfo: ImplementationInfo{
			Name:    c.name,
			Version: ClientVersion,
		},
	}

	resp, err := c.transport.SendRequest(ctx, "initialize", params)
	if err != nil {
		c.transport.Close()
		return fmt.Errorf("initialize failed: %w", err)
	}

	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("marshaling initialize result: %w", err)
	}

	var initResult InitializeResult
	if err := json.Unmarshal(resultBytes, &initResult); err != nil {
		return fmt.Errorf("parsing initialize result: %w", err)
	}

	c.serverInfo = initResult.ServerInfo
	c.logger.Info("MCP client initialized",
		"server", initResult.ServerInfo.Name,
		"version", initResult.ServerInfo.Version,
		"protocol", initResult.ProtocolVersion)

	if err := c.transport.SendNotification("notifications/initialized", nil); err != nil {
		c.logger.Warn("failed to send initialized notification", "err", err)
	}

	c.initialized = true
	return nil
}

func (c *Client) RefreshTools(ctx context.Context) ([]Tool, error) {
	resp, err := c.transport.SendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}

	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshaling tools result: %w", err)
	}

	var listResult ListToolsResult
	if err := json.Unmarshal(resultBytes, &listResult); err != nil {
		return nil, fmt.Errorf("parsing tools result: %w", err)
	}

	c.mu.Lock()
	c.tools = listResult.Tools
	c.toolsMap = make(map[string]Tool, len(listResult.Tools))
	for _, tool := range listResult.Tools {
		c.toolsMap[tool.Name] = tool
	}
	c.mu.Unlock()

	c.logger.Info("MCP tools refreshed", "count", len(listResult.Tools))
	return listResult.Tools, nil
}

func (c *Client) ListTools() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Tool, len(c.tools))
	copy(result, c.tools)
	return result
}

func (c *Client) GetTool(name string) (Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.toolsMap[name]
	return t, ok
}

func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*CallToolResult, error) {
	if !c.initialized {
		return nil, fmt.Errorf("client not initialized")
	}

	c.mu.RLock()
	_, known := c.toolsMap[name]
	c.mu.RUnlock()

	if !known {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}

	params := CallToolParams{
		Name:      name,
		Arguments: arguments,
	}

	resp, err := c.transport.SendRequest(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("tools/call %s failed: %w", name, err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("tool %s error: %s (code %d)", name, resp.Error.Message, resp.Error.Code)
	}

	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshaling tool result: %w", err)
	}

	var callResult CallToolResult
	if err := json.Unmarshal(resultBytes, &callResult); err != nil {
		return nil, fmt.Errorf("parsing tool result: %w", err)
	}

	return &callResult, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.transport.SendRequest(ctx, "ping", nil)
	return err
}

func (c *Client) ServerInfo() ImplementationInfo {
	return c.serverInfo
}

func (c *Client) ServerName() string {
	return c.serverInfo.Name
}

func (c *Client) Ready() bool {
	return c.initialized && c.transport.Ready()
}

func (c *Client) Close() error {
	c.initialized = false
	return c.transport.Close()
}
