package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/util"
)

var (
	ErrTransportClosed           = errors.New("mcp transport: closed")
	ErrTransportNotReady         = errors.New("mcp transport: not connected")
	ErrTransportAlreadyConnected = errors.New("mcp transport: already connected")
	ErrRequestTimeout            = errors.New("mcp transport: request timeout")
)

type TransportConfig struct {
	URL            string
	Headers        map[string]string
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
	// ReconnectBackoff is reserved for future automatic reconnection.
	// Currently unused: transports do not reconnect automatically.
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

	mu        sync.Mutex
	closed    atomic.Bool
	ready     atomic.Bool
	gwMetrics *infra.Metrics

	sessionEndpoint string
	baseHost        string
	sseCancel       context.CancelFunc
	sseBody         io.ReadCloser
	readLoopStarted atomic.Bool
	// connecting guards against concurrent Connect calls; guarded by mu.
	connecting bool

	// streamCtx owns the SSE stream lifetime: derived from Background so a
	// short-lived caller context (e.g. Initialize's) cannot kill the stream
	// after Connect returns. Canceled only by Close.
	streamCtx    context.Context
	streamCancel context.CancelFunc

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
	t.streamCtx, t.streamCancel = context.WithCancel(context.Background())

	t.httpClient = &http.Client{
		Timeout: cfg.RequestTimeout + 5*time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: cfg.RequestTimeout,
			IdleConnTimeout:       cfg.IdleTimeout,
			MaxIdleConns:          2,
			MaxIdleConnsPerHost:   2,
		},
	}

	return t
}

// SetGatewayMetrics sets the metrics instance for tracking MCP goroutines.
// Must be called before Connect: afterwards the transport's goroutines read
// the field concurrently.
func (t *HTTPTransport) SetGatewayMetrics(metrics *infra.Metrics) {
	t.gwMetrics = metrics
}

func (t *HTTPTransport) trackGoroutineStart() {
	if t.gwMetrics != nil {
		t.gwMetrics.IncMCPActiveGoroutines()
	}
}

func (t *HTTPTransport) trackGoroutineEnd() {
	if t.gwMetrics != nil {
		t.gwMetrics.DecMCPActiveGoroutines()
	}
}

// reserveConnectSlot atomically reserves the single Connect slot, rejecting
// closed, already-connected, or concurrently-connecting transports.
func (t *HTTPTransport) reserveConnectSlot() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closed.Load():
		return ErrTransportClosed
	case t.sseBody != nil || t.connecting:
		return ErrTransportAlreadyConnected
	default:
		t.connecting = true
		return nil
	}
}

func (t *HTTPTransport) Connect(ctx context.Context) error {
	// Reserve the connect slot atomically: concurrent Connect calls must not
	// both reach the stream spawn (double doneCh close = process crash).
	if err := t.reserveConnectSlot(); err != nil {
		return err
	}
	// Every failure exit below must release the slot via connectFailed.
	connectFailed := func() {
		t.mu.Lock()
		t.connecting = false
		t.mu.Unlock()
	}

	baseURL, err := url.Parse(t.cfg.URL)
	if err != nil {
		connectFailed()
		return fmt.Errorf("invalid MCP server URL: %w", err)
	}

	sseURL := baseURL.String()
	if !strings.HasSuffix(sseURL, "/sse") && !strings.HasSuffix(sseURL, "/") {
		sseURL += "/sse"
	} else if strings.HasSuffix(sseURL, "/") {
		sseURL += "sse"
	}

	t.mu.Lock()
	t.baseHost = baseURL.Host
	t.mu.Unlock()

	t.cfg.Logger.Debug("connecting to MCP SSE endpoint", "url", sseURL)

	// The SSE stream lifetime is owned by the transport (streamCtx, canceled
	// only by Close) so a short-lived caller context (e.g. Initialize's)
	// cannot kill the stream after Connect returns. The per-request client
	// timeout is likewise bypassed — it would kill the stream mid-session.
	sseCtx, sseCancel := context.WithCancel(t.streamCtx)
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, sseURL, http.NoBody)
	if err != nil {
		sseCancel()
		connectFailed()
		return fmt.Errorf("creating SSE request: %w", err)
	}
	t.setHeaders(req)

	// connectCtx (caller-bounded) paces only the dial/retry phase; the
	// handshake read below is bounded separately via a read deadline.
	connectCtx, connectCancel := context.WithTimeout(ctx, t.cfg.ConnectTimeout)
	defer connectCancel()

	// SSE GET uses a client without Timeout: the stream lives for the
	// connection lifetime (sseCtx), while the shared client's Timeout would
	// forcibly kill it mid-session.
	sseClient := &http.Client{Transport: t.httpClient.Transport}

	// Body intentionally kept open for SSE reading; closed via t.sseBody in sseReadLoop/Close().
	resp, err := util.DoWithRetryResp(connectCtx, 3, func() (*http.Response, error) { //nolint:bodyclose // SSE body lifecycle spans goroutines
		var fetchErr error
		resp, fetchErr := sseClient.Do(req)
		if fetchErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return nil, fetchErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("SSE connection returned status %d", resp.StatusCode)
		}
		return resp, nil
	})
	if err != nil {
		sseCancel()
		connectFailed()
		return fmt.Errorf("SSE connection failed: %w", err)
	}

	t.mu.Lock()
	t.sessionEndpoint = ""
	t.sseBody = resp.Body
	t.mu.Unlock()

	sseReader := bufio.NewReader(resp.Body)

	// Bound the SSE handshake (headers + first endpoint event) even when the
	// server stalls mid-line: the reader itself is not context-aware, so the
	// read runs in a helper goroutine and connectCtx bounds the wait; the
	// error path below closes the body, which unblocks a parked Read.
	endpointURL, err := t.readEndpointWithTimeout(connectCtx, sseReader, baseURL)
	if err != nil {
		sseCancel()
		t.mu.Lock()
		body := t.sseBody
		t.sseBody = nil
		t.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		connectFailed()
		return err
	}

	// Install the stream only if Close has not won the race.
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		sseCancel()
		_ = resp.Body.Close()
		connectFailed()
		return ErrTransportClosed
	}
	t.sessionEndpoint = endpointURL
	t.sseCancel = sseCancel
	t.mu.Unlock()

	t.cfg.Logger.Debug("received MCP session endpoint", "endpoint", endpointURL)

	t.readLoopStarted.Store(true)
	go func() {
		t.trackGoroutineStart()
		t.sseReadLoop(sseReader)
		t.trackGoroutineEnd()
	}()
	go func() {
		t.trackGoroutineStart()
		t.eventDispatchLoop()
		t.trackGoroutineEnd()
	}()
	go func() {
		t.trackGoroutineStart()
		t.keepaliveLoop()
		t.trackGoroutineEnd()
	}()

	t.mu.Lock()
	t.connecting = false
	t.mu.Unlock()
	t.ready.Store(true)
	return nil
}

// readEndpointWithTimeout bounds readSSEInitialEndpoint with ctx even though
// the underlying ReadString cannot be interrupted: on ctx expiry the helper
// returns immediately and the caller closes the body, which unblocks the
// helper goroutine (result channel is buffered so it never leaks).
func (t *HTTPTransport) readEndpointWithTimeout(ctx context.Context, reader *bufio.Reader, baseURL *url.URL) (string, error) {
	type endpointResult struct {
		endpoint string
		err      error
	}
	ch := make(chan endpointResult, 1)
	go func() {
		endpoint, err := readSSEInitialEndpoint(ctx, reader, baseURL, t.cfg.Logger)
		ch <- endpointResult{endpoint, err}
	}()
	select {
	case r := <-ch:
		return r.endpoint, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for MCP session endpoint: %w", ctx.Err())
	}
}

func readSSEInitialEndpoint(ctx context.Context, reader *bufio.Reader, baseURL *url.URL, logger *slog.Logger) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for MCP session endpoint: %w", ctx.Err())
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("reading SSE endpoint event: %w", err)
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
		if jsonErr := json.Unmarshal([]byte(data), &parsed); jsonErr != nil {
			logger.Debug("non-JSON SSE data, treating as endpoint", "data", data)
			parsed.Endpoint = data
		}

		if parsed.Endpoint == "" {
			continue
		}

		endpointURL := parsed.Endpoint
		if !strings.HasPrefix(endpointURL, "http") {
			return baseURL.Scheme + "://" + baseURL.Host + endpointURL, nil
		}

		epURL, err := url.Parse(endpointURL)
		if err != nil || epURL.Host != baseURL.Host {
			gotHost := ""
			if err == nil {
				gotHost = epURL.Host
			}
			logger.Warn("MCP server sent endpoint with unexpected host, rejecting",
				"expected_host", baseURL.Host, "got_host", gotHost, "endpoint", endpointURL)
			return "", fmt.Errorf("MCP session endpoint host mismatch: expected %s, got %s", baseURL.Host, gotHost)
		}

		return endpointURL, nil
	}
}

func (t *HTTPTransport) sendHTTPPost(ctx context.Context, client *http.Client, endpoint string, reqBytes []byte) (*http.Response, error) {
	return util.DoWithRetryResp(ctx, 2, func() (*http.Response, error) {
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(reqBytes)))
		if reqErr != nil {
			return nil, fmt.Errorf("creating POST request: %w", reqErr)
		}
		t.setHeaders(httpReq)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, doErr := client.Do(httpReq)
		if doErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return nil, doErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return resp, nil
	})
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

	httpResp, err := t.sendHTTPPost(postCtx, t.httpClient, endpoint, reqBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("POST to MCP endpoint failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

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

func (t *HTTPTransport) SendNotification(ctx context.Context, method string, params any) error {
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

	// Notifications are decoupled from any HTTP request; bounded by
	// RequestTimeout. ctx must be non-nil (§5: contexts are never repaired).
	reqCtx, cancel := context.WithTimeout(ctx, t.cfg.RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(reqBytes)))
	if err != nil {
		return fmt.Errorf("creating notification request: %w", err)
	}
	t.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))

	return nil
}

func (t *HTTPTransport) Ready() bool {
	return t.ready.Load() && !t.closed.Load()
}

func (t *HTTPTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.ready.Store(false)

		t.mu.Lock()
		sseCancel := t.sseCancel
		t.mu.Unlock()

		if sseCancel != nil {
			sseCancel()
		}
		// Kill the stream lifetime context: any Connect still mid-flight is
		// torn down and future streams cannot outlive the transport.
		t.streamCancel()
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

		// The done channel is only closed by the SSE read loop; skip the
		// grace wait when that loop never started (Connect failed or was
		// never called) — otherwise every unconnected client stalls
		// shutdown for the full grace period, sequentially per client.
		if t.readLoopStarted.Load() {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-t.doneCh:
			case <-timer.C:
			}
			timer.Stop()
		}

		// Re-snapshot: Connect may have installed the stream after the first
		// snapshot (Close raced a mid-flight Connect). Cancel and close
		// outside t.mu — body.Close can block on a parked reader.
		t.mu.Lock()
		if t.sseCancel != nil {
			t.sseCancel()
		}
		sseBody := t.sseBody
		t.sseBody = nil
		t.mu.Unlock()
		if sseBody != nil {
			_ = sseBody.Close()
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
				continue
			}
			pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := t.SendNotification(pingCtx, "ping", nil); err != nil {
				cancel()
				if !t.closed.Load() {
					t.cfg.Logger.Warn("MCP keepalive ping failed", "err", err)
					t.ready.Store(false)
				}
				return
			}
			cancel()
			// A transient blip must not brick the client for its lifetime:
			// a successful ping restores readiness.
			t.ready.Store(true)
		}
	}
}

func readMultiLineEvent(reader *bufio.Reader) string {
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
			return strings.TrimPrefix(nextLine, "data: ")
		}
	}
	return ""
}

func (t *HTTPTransport) sseReadLoop(reader *bufio.Reader) {
	defer close(t.doneCh)
	defer func() {
		t.mu.Lock()
		body := t.sseBody
		t.sseBody = nil
		t.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
	}()

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
			eventData = readMultiLineEvent(reader)
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
				endpointURL, ok := t.parseAndValidateEndpoint(event.Data)
				if !ok {
					continue
				}
				t.mu.Lock()
				t.sessionEndpoint = endpointURL
				t.mu.Unlock()
				t.ready.Store(true)
				continue
			}

			var rpcResp Response
			if err := json.Unmarshal([]byte(event.Data), &rpcResp); err != nil {
				t.cfg.Logger.Debug("ignoring non-JSON SSE event", "event", event.Event, "data", event.Data)
				continue
			}

			dispatchJSONRPCResponse(t, rpcResp)
		}
	}
}

func dispatchJSONRPCResponse(t *HTTPTransport, rpcResp Response) {
	if rpcResp.ID == nil {
		return
	}

	var idKey int64
	switch id := rpcResp.ID.(type) {
	case float64:
		idKey = int64(id)
	case int64:
		idKey = id
	default:
		return
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

func (t *HTTPTransport) SessionEndpoint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionEndpoint
}

// parseAndValidateEndpoint validates the dynamic MCP endpoint event: the
// endpoint must share the SSE connection's host.
func (t *HTTPTransport) parseAndValidateEndpoint(data string) (string, bool) {
	var parsed struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return "", false
	}
	if parsed.Endpoint == "" {
		return "", false
	}
	epURL, err := url.Parse(parsed.Endpoint)
	if err != nil {
		return "", false
	}
	t.mu.Lock()
	baseHost := t.baseHost
	t.mu.Unlock()
	if epURL.Host != baseHost {
		gotHost := epURL.Host
		t.cfg.Logger.Warn("MCP server sent dynamic endpoint with unexpected host, rejecting",
			"expected_host", baseHost, "got_host", gotHost, "endpoint", parsed.Endpoint)
		return "", false
	}
	return parsed.Endpoint, true
}
