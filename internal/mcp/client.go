package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nenya/internal/infra"
)

const ClientVersion = "0.1.0"

// Client communicates with an MCP server over HTTP+SSE transport.
// It handles initialization, tool discovery, and tool invocation.
type Client struct {
	transport *HTTPTransport
	name      string
	logger    *slog.Logger
	cfg       ClientConfig
	metrics   *infra.Metrics

	mu          sync.RWMutex
	tools       []Tool
	toolsMap    map[string]Tool
	initialized atomic.Bool
	serverInfo  ImplementationInfo
	// recoveryGen counts completed session recoveries; recoverMu makes
	// recovery single-flight so concurrent broken-session callers trigger
	// exactly one rebuild and then each replay their call once.
	recoverMu   sync.Mutex
	recoveryGen uint64
}

// ClientConfig holds the parameters for creating a new MCP Client.
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

// NewClient creates a new MCP client with the given configuration and
// initiates the SSE transport connection.
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
		cfg:       cfg,
		toolsMap:  make(map[string]Tool),
	}
}

// currentTransport returns the live transport under a read lock, so
// callers never race the swap performed by session recovery.
func (c *Client) currentTransport() *HTTPTransport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.transport
}

func (c *Client) Initialize(ctx context.Context) error {
	tr := c.currentTransport()
	if err := tr.Connect(ctx); err != nil {
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

	resp, err := tr.SendRequest(ctx, "initialize", params)
	if err != nil {
		_ = tr.Close()
		return fmt.Errorf("initialize failed: %w", err)
	}

	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		// A half-initialized client can never recover (single-use transport):
		// tear the stream down instead of leaking it.
		_ = tr.Close()
		return fmt.Errorf("marshaling initialize result: %w", err)
	}

	var initResult InitializeResult
	if err := json.Unmarshal(resultBytes, &initResult); err != nil {
		_ = tr.Close()
		return fmt.Errorf("parsing initialize result: %w", err)
	}

	c.mu.Lock()
	c.serverInfo = initResult.ServerInfo
	c.mu.Unlock()
	c.logger.Info("MCP client initialized",
		"server", initResult.ServerInfo.Name,
		"version", initResult.ServerInfo.Version,
		"protocol", initResult.ProtocolVersion)

	if err := tr.SendNotification(ctx, "notifications/initialized", nil); err != nil {
		c.logger.Warn("failed to send initialized notification", "err", err)
	}

	c.initialized.Store(true)
	return nil
}

func (c *Client) RefreshTools(ctx context.Context) ([]Tool, error) {
	resp, err := c.currentTransport().SendRequest(ctx, "tools/list", nil)
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
	return c.tools
}

func (c *Client) GetTool(name string) (Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.toolsMap[name]
	return t, ok
}

func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*CallToolResult, error) {
	if !c.initialized.Load() {
		// Not initialized: either never initialized or a concurrent
		// recovery is mid-flight. Route through Recover — single-flight,
		// so this joins the in-flight rebuild instead of failing.
		if err := c.Recover(ctx); err != nil {
			return nil, fmt.Errorf("client not initialized: %w", err)
		}
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

	resp, err := c.currentTransport().SendRequest(ctx, "tools/call", params)
	if err != nil {
		// Transport-level failure: the server may have restarted or
		// expired the session. Recover single-flight, then replay the
		// call exactly once — on the transport current after recovery.
		if recErr := c.Recover(ctx); recErr != nil {
			return nil, fmt.Errorf("tools/call %s failed: %w (recovery failed: %v)", name, err, recErr)
		}
		resp, err = c.currentTransport().SendRequest(ctx, "tools/call", params)
		if err != nil {
			return nil, fmt.Errorf("tools/call %s failed after session recovery: %w", name, err)
		}
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
	_, err := c.currentTransport().SendRequest(ctx, "ping", nil)
	return err
}

func (c *Client) ServerInfo() ImplementationInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo
}

func (c *Client) ServerName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo.Name
}

func (c *Client) Ready() bool {
	return c.initialized.Load() && c.currentTransport().Ready()
}

// Recover rebuilds the transport and re-runs the initialization handshake
// after a broken or expired server session (server restart, endpoint
// rotation). Recovery is single-flight: the first caller performs the
// rebuild while concurrent callers block on the same attempt; once it
// completes, every waiter replays its in-flight call exactly once
// (CallTool). Bounded by ctx — including the 5-minute multi-turn loop
// deadline. A failed recovery leaves the client untouched for a later
// retry.
func (c *Client) Recover(ctx context.Context) error {
	c.mu.Lock()
	gen := c.recoveryGen
	c.mu.Unlock()

	c.recoverMu.Lock()
	defer c.recoverMu.Unlock()

	// Another goroutine already recovered this generation: the replay on
	// the fresh transport is all this caller needs.
	c.mu.RLock()
	done := c.recoveryGen > gen && c.initialized.Load() && c.transport.Ready()
	c.mu.RUnlock()
	if done {
		return nil
	}

	if err := c.reinitialize(ctx); err != nil {
		return err
	}

	c.mu.Lock()
	c.recoveryGen++
	c.mu.Unlock()
	return nil
}

// reinitialize swaps in a fresh transport and re-runs the handshake. The
// old transport is closed first; a half-initialized rebuild tears the new
// stream down via Initialize's error path.
func (c *Client) reinitialize(ctx context.Context) error {
	_ = c.transport.Close()
	c.initialized.Store(false)

	c.transport = NewHTTPTransport(TransportConfig{
		URL:               c.cfg.URL,
		Headers:           c.cfg.Headers,
		ConnectTimeout:    c.cfg.ConnectTimeout,
		RequestTimeout:    c.cfg.RequestTimeout,
		IdleTimeout:       c.cfg.IdleTimeout,
		KeepAliveInterval: c.cfg.KeepAliveInterval,
		Logger:            c.logger,
	})
	if c.metrics != nil {
		c.transport.SetGatewayMetrics(c.metrics)
	}
	return c.Initialize(ctx)
}

func (c *Client) Close() error {
	c.initialized.Store(false)
	return c.currentTransport().Close()
}

// SetGatewayMetrics sets the metrics instance for tracking MCP transport
// goroutines. Must be called before Connect/Initialize: afterwards the
// transport's goroutines read the field concurrently. The instance is
// retained so session recovery re-attaches it to the rebuilt transport.
func (c *Client) SetGatewayMetrics(metrics *infra.Metrics) {
	c.metrics = metrics
	c.transport.SetGatewayMetrics(metrics)
}
