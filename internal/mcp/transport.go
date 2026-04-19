package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrTransportClosed   = errors.New("mcp transport: closed")
	ErrTransportNotReady = errors.New("mcp transport: not connected")
	ErrRequestTimeout    = errors.New("mcp transport: request timeout")
)

type TransportConfig struct {
	URL               string
	Headers           map[string]string
	ConnectTimeout    time.Duration
	RequestTimeout    time.Duration
	IdleTimeout       time.Duration
	ReconnectBackoff  time.Duration
	KeepAliveInterval time.Duration
	Logger            *slog.Logger
}

func (c *TransportConfig) setDefaults() {
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.ReconnectBackoff <= 0 {
		c.ReconnectBackoff = 30 * time.Second
	}
	if c.KeepAliveInterval <= 0 {
		c.KeepAliveInterval = 4 * time.Second
	}
}

type HTTPTransport struct {
	cfg        TransportConfig
	httpClient *http.Client

	mu     sync.Mutex
	closed atomic.Bool
	ready  atomic.Bool

	sessionEndpoint string
	sseCancel       context.CancelFunc

	pendingMu sync.Mutex
	pending   map[int64]chan *Response
	nextID    atomic.Int64

	eventCh   chan sseEvent
	closeCh   chan struct{}
	closeOnce sync.Once
	doneCh    chan struct{}
}

type sseEvent struct {
	Event string
	Data  string
}

func NewHTTPTransport(cfg TransportConfig) *HTTPTransport {
	cfg.setDefaults()

	t := &HTTPTransport{
		cfg:     cfg,
		pending: make(map[int64]chan *Response),
		eventCh: make(chan sseEvent, 64),
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	t.httpClient = &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: cfg.RequestTimeout,
			IdleConnTimeout:       cfg.IdleTimeout,
			MaxIdleConns:          2,
			MaxIdleConnsPerHost:   2,
		},
	}

	return t
}

func (t *HTTPTransport) Connect(ctx context.Context) error {
	baseURL, err := url.Parse(t.cfg.URL)
	if err != nil {
		return fmt.Errorf("invalid MCP server URL: %w", err)
	}

	sseURL := baseURL.String()
	if !strings.HasSuffix(sseURL, "/sse") && !strings.HasSuffix(sseURL, "/") {
		sseURL += "/sse"
	} else if strings.HasSuffix(sseURL, "/") {
		sseURL += "sse"
	}

	t.cfg.Logger.Debug("connecting to MCP SSE endpoint", "url", sseURL)

	sseCtx, sseCancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, sseURL, nil)
	if err != nil {
		sseCancel()
		return fmt.Errorf("creating SSE request: %w", err)
	}
	t.setHeaders(req)

	// Apply timeout only to the initial connection and endpoint event reading
	connectCtx, connectCancel := context.WithTimeout(sseCtx, t.cfg.ConnectTimeout)
	defer connectCancel()

	resp, err := t.httpClient.Do(req)
	if err != nil {
		sseCancel()
		return fmt.Errorf("SSE connection failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		sseCancel()
		return fmt.Errorf("SSE connection returned status %d", resp.StatusCode)
	}

	t.mu.Lock()
	t.sessionEndpoint = ""
	t.mu.Unlock()

	sseReader := bufio.NewReader(resp.Body)

	for {
		select {
		case <-connectCtx.Done():
			sseCancel()
			resp.Body.Close()
			return fmt.Errorf("waiting for MCP session endpoint: %w", connectCtx.Err())
		default:
		}

		line, err := sseReader.ReadString('\n')
		if err != nil {
			sseCancel()
			resp.Body.Close()
			return fmt.Errorf("reading SSE endpoint event: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)

		var parsed struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			t.cfg.Logger.Debug("non-JSON SSE data, treating as endpoint", "data", data)
			parsed.Endpoint = data
		}

		if parsed.Endpoint != "" {
			endpointURL := parsed.Endpoint
			if !strings.HasPrefix(endpointURL, "http") {
				endpointURL = baseURL.Scheme + "://" + baseURL.Host + endpointURL
			}

			t.mu.Lock()
			t.sessionEndpoint = endpointURL
			t.mu.Unlock()

			t.cfg.Logger.Debug("received MCP session endpoint", "endpoint", endpointURL)

			t.sseCancel = sseCancel
			go t.sseReadLoop(sseReader)
			go t.eventDispatchLoop()
			go t.keepaliveLoop()

			break
		}
	}

	t.ready.Store(true)
	return nil
}

func (t *HTTPTransport) SendRequest(ctx context.Context, method string, params any) (*Response, error) {
	if t.closed.Load() {
		return nil, ErrTransportClosed
	}
	if !t.ready.Load() {
		return nil, ErrTransportNotReady
	}

	id := t.nextID.Add(1)
	req := Request{
		JSONRPC: JSONRPCVersion2,
		ID:      id,
		Method:  method,
		Params:  params,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	t.pendingMu.Lock()
	ch := make(chan *Response, 1)
	t.pending[id] = ch
	t.pendingMu.Unlock()

	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
	}()

	t.mu.Lock()
	endpoint := t.sessionEndpoint
	t.mu.Unlock()

	if endpoint == "" {
		return nil, ErrTransportNotReady
	}

	postCtx, cancel := context.WithTimeout(ctx, t.cfg.RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(postCtx, http.MethodPost, endpoint, strings.NewReader(string(reqBytes)))
	if err != nil {
		return nil, fmt.Errorf("creating POST request: %w", err)
	}
	t.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("POST to MCP endpoint failed: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading MCP response: %w", err)
	}

	if httpResp.StatusCode == http.StatusAccepted {
		return t.waitForJSONRPCResponse(postCtx, ch)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP endpoint returned status %d: %s", httpResp.StatusCode, string(body))
	}

	var rpcResp Response
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshaling MCP response: %w", err)
	}

	if rpcResp.ID == nil {
		return &rpcResp, nil
	}

	respID, ok := rpcResp.ID.(float64)
	if ok && int64(respID) == id {
		if rpcResp.Error != nil {
			return &rpcResp, rpcResp.Error
		}
		return &rpcResp, nil
	}

	return t.waitForJSONRPCResponse(postCtx, ch)
}

func (t *HTTPTransport) waitForJSONRPCResponse(ctx context.Context, ch chan *Response) (*Response, error) {
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *HTTPTransport) SendNotification(method string, params any) error {
	if t.closed.Load() || !t.ready.Load() {
		return ErrTransportClosed
	}

	notif := Notification{
		JSONRPC: JSONRPCVersion2,
		Method:  method,
		Params:  params,
	}

	reqBytes, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshaling notification: %w", err)
	}

	t.mu.Lock()
	endpoint := t.sessionEndpoint
	t.mu.Unlock()

	if endpoint == "" {
		return ErrTransportNotReady
	}

	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(reqBytes)))
	if err != nil {
		return fmt.Errorf("creating notification request: %w", err)
	}
	t.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(io.LimitReader(resp.Body, 4096))

	return nil
}

func (t *HTTPTransport) Ready() bool {
	return t.ready.Load() && !t.closed.Load()
}

func (t *HTTPTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.ready.Store(false)

		if t.sseCancel != nil {
			t.sseCancel()
		}
		close(t.closeCh)

		t.pendingMu.Lock()
		for id, ch := range t.pending {
			ch <- &Response{
				JSONRPC: JSONRPCVersion2,
				Error:   &Error{Code: ErrCodeInternal, Message: "transport closed"},
			}
			delete(t.pending, id)
		}
		t.pendingMu.Unlock()

		select {
		case <-t.doneCh:
		case <-time.After(5 * time.Second):
		}
	})
	return nil
}

func (t *HTTPTransport) setHeaders(req *http.Request) {
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
}

func (t *HTTPTransport) keepaliveLoop() {
	ticker := time.NewTicker(t.cfg.KeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.closeCh:
			return
		case <-ticker.C:
			if !t.ready.Load() {
				return
			}
			if err := t.SendNotification("ping", nil); err != nil {
				if !t.closed.Load() {
					t.cfg.Logger.Warn("MCP keepalive ping failed", "err", err)
					t.ready.Store(false)
				}
				return
			}
		}
	}
}

func (t *HTTPTransport) sseReadLoop(reader *bufio.Reader) {
	defer close(t.doneCh)

	for {
		select {
		case <-t.closeCh:
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if !t.closed.Load() {
				t.cfg.Logger.Warn("SSE connection lost, marking transport as not ready", "err", err)
				t.ready.Store(false)
			}
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var eventType, eventData string

		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
			for {
				nextLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					break
				}
				nextLine = strings.TrimSpace(nextLine)
				if nextLine == "" {
					break
				}
				if strings.HasPrefix(nextLine, "data: ") {
					eventData = strings.TrimPrefix(nextLine, "data: ")
				}
			}
		case strings.HasPrefix(line, "data: "):
			eventData = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, ":"):
		default:
			continue
		}

		if eventData == "" {
			continue
		}

		select {
		case t.eventCh <- sseEvent{Event: eventType, Data: eventData}:
		case <-t.closeCh:
			return
		}
	}
}

func (t *HTTPTransport) eventDispatchLoop() {
	for {
		select {
		case <-t.closeCh:
			return
		case event, ok := <-t.eventCh:
			if !ok {
				return
			}

			if event.Data == "" {
				continue
			}

			if event.Event == "endpoint" {
				var parsed struct {
					Endpoint string `json:"endpoint"`
				}
				if err := json.Unmarshal([]byte(event.Data), &parsed); err == nil && parsed.Endpoint != "" {
					t.mu.Lock()
					t.sessionEndpoint = parsed.Endpoint
					t.mu.Unlock()
					t.ready.Store(true)
					continue
				}
			}

			var rpcResp Response
			if err := json.Unmarshal([]byte(event.Data), &rpcResp); err != nil {
				t.cfg.Logger.Debug("ignoring non-JSON SSE event", "event", event.Event, "data", event.Data)
				continue
			}

			if rpcResp.ID == nil {
				continue
			}

			var idKey int64
			switch id := rpcResp.ID.(type) {
			case float64:
				idKey = int64(id)
			case int64:
				idKey = id
			default:
				continue
			}

			t.pendingMu.Lock()
			ch, ok := t.pending[idKey]
			if ok {
				delete(t.pending, idKey)
			}
			t.pendingMu.Unlock()

			if ok {
				select {
				case ch <- &rpcResp:
				default:
					t.cfg.Logger.Warn("dropping response for pending request, channel full", "id", idKey)
				}
			}
		}
	}
}

func (t *HTTPTransport) SessionEndpoint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionEndpoint
}
