package mcp

import (
	"bufio"
	"bytes"
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

// Sentinel errors returned by HTTPTransport methods.
var (
	// ErrTransportClosed is returned when the transport has been closed.
	ErrTransportClosed = errors.New("mcp transport: closed")
	// ErrTransportNotReady is returned when no live SSE stream is available.
	ErrTransportNotReady = errors.New("mcp transport: not ready")
	// ErrTransportAlreadyConnected is returned by Connect when a stream was
	// already established or a connection attempt is in flight; transports
	// are single-use.
	ErrTransportAlreadyConnected = errors.New("mcp transport: already connected")
)

// TransportConfig configures an HTTPTransport.
type TransportConfig struct {
	URL               string
	Headers           map[string]string
	ConnectTimeout    time.Duration
	RequestTimeout    time.Duration
	IdleTimeout       time.Duration
	KeepAliveInterval time.Duration
	Logger            *slog.Logger
}

func (c *TransportConfig) setDefaults() {
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.KeepAliveInterval <= 0 {
		c.KeepAliveInterval = 4 * time.Second
	}
}

// HTTPTransport is the HTTP+SSE implementation of the MCP client transport
// wire protocol.
// A transport is single-use once connected: it serves exactly one SSE stream
// for its lifetime and is torn down by Close. A Connect attempt that fails
// before the stream is established may be retried.
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

// NewHTTPTransport creates a transport for the given config. Call Connect to
// establish the SSE stream; Close tears it down.
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
// connectTransport returns a transport for the connect phase only: the
// shared transport cloned with dial/TLS/header timeouts tightened to
// ConnectTimeout. ResponseHeaderTimeout bounds stalled servers without
// affecting the established stream's body lifetime.
func (t *HTTPTransport) connectTransport() http.RoundTripper {
	tr, ok := t.httpClient.Transport.(*http.Transport)
	if !ok {
		return t.httpClient.Transport
	}
	clone := tr.Clone()
	clone.ResponseHeaderTimeout = t.cfg.ConnectTimeout
	if clone.TLSHandshakeTimeout > t.cfg.ConnectTimeout {
		clone.TLSHandshakeTimeout = t.cfg.ConnectTimeout
	}
	if clone.DialContext != nil {
		baseDial := clone.DialContext
		clone.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, t.cfg.ConnectTimeout)
			defer cancel()
			return baseDial(dialCtx, network, addr)
		}
	}
	return clone
}

func (t *HTTPTransport) reserveConnectSlot() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closed.Load():
		return ErrTransportClosed
	case t.sseBody != nil || t.connecting || t.readLoopStarted.Load():
		// readLoopStarted makes the transport single-use: after the stream
		// dies (sseReadLoop nils sseBody) a re-Connect would spawn a second
		// sseReadLoop and double-close doneCh — rejected instead.
		return ErrTransportAlreadyConnected
	default:
		t.connecting = true
		return nil
	}
}

// Connect establishes the SSE stream, performs the endpoint handshake, and
// spawns the read/dispatch/keepalive goroutines. The stream's lifetime is
// owned by the transport (torn down by Close), not by ctx; ctx bounds only
// the connect phase. A second call after a successful connect (or while one
// is in flight, or after the stream died) returns ErrTransportAlreadyConnected;
// after Close it returns ErrTransportClosed. A failed connect attempt (no
// stream established) may be retried.
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

	// Build the SSE URL via path joining: string suffix checks would mangle
	// URLs carrying query strings or fragments.
	sseURL := *baseURL
	switch {
	case strings.HasSuffix(sseURL.Path, "/sse"):
		// Already an SSE endpoint; leave path (and query) untouched.
	case strings.HasSuffix(sseURL.Path, "/"):
		sseURL.Path += "sse"
	default:
		if sseURL.Path == "" {
			sseURL.Path = "/sse"
		} else {
			sseURL.Path += "/sse"
		}
	}
	sseURLStr := sseURL.String()

	t.mu.Lock()
	t.baseHost = baseURL.Host
	t.mu.Unlock()

	t.cfg.Logger.Debug("connecting to MCP SSE endpoint", "url", sseURLStr)

	// The SSE stream lifetime is owned by the transport (streamCtx, canceled
	// only by Close) so a short-lived caller context (e.g. Initialize's)
	// cannot kill the stream after Connect returns. The per-request client
	// timeout is likewise bypassed — it would kill the stream mid-session.
	sseCtx, sseCancel := context.WithCancel(t.streamCtx)
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, sseURLStr, http.NoBody)
	if err != nil {
		sseCancel()
		connectFailed()
		return fmt.Errorf("creating SSE request: %w", err)
	}
	t.setHeaders(req)

	// connectCtx (caller-bounded) bounds the total connect phase including
	// the endpoint-event read below (via readEndpointWithTimeout). The SSE
	// dial/header wait itself rides sseCtx (no client Timeout) and is bounded
	// by the shared transport's Dialer.Timeout/ResponseHeaderTimeout.
	connectCtx, connectCancel := context.WithTimeout(ctx, t.cfg.ConnectTimeout)
	defer connectCancel()

	// SSE GET uses a client without Timeout: the stream lives for the
	// connection lifetime (sseCtx), while the shared client's Timeout would
	// forcibly kill it mid-session. The connect-scoped transport bounds the
	// dial/header wait by ConnectTimeout — ResponseHeaderTimeout stops
	// time-to-headers only, so the established stream is not affected.
	connectClient := &http.Client{Transport: t.connectTransport()}

	// Body intentionally kept open for SSE reading; closed via t.sseBody in sseReadLoop/Close().
	resp, err := util.DoWithRetryResp(connectCtx, 3, func() (*http.Response, error) { //nolint:bodyclose // SSE body lifecycle spans goroutines
		var fetchErr error
		resp, fetchErr := connectClient.Do(req)
		if fetchErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return nil, fetchErr
		}
		if resp.StatusCode >= 500 {
			// Retryable upstream failure.
			_ = resp.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			// 4xx and other statuses are terminal (§8: 4xx is never
			// retried): hand the response back so the caller fails once.
			return resp, nil
		}
		return resp, nil
	})
	if err != nil {
		sseCancel()
		connectFailed()
		return fmt.Errorf("SSE connection failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Terminal non-200 (e.g. 401/403): not retried, fail once.
		_ = resp.Body.Close()
		sseCancel()
		connectFailed()
		return fmt.Errorf("SSE connection returned status %d", resp.StatusCode)
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
		t.abortHandshake(sseCancel, connectFailed)
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

	return t.completeConnect()
}

// completeConnect finalizes a successful connect: releases the connect slot,
// marks the transport ready, and reports failure instead of success when
// Close has won the race.
func (t *HTTPTransport) completeConnect() error {
	t.mu.Lock()
	t.connecting = false
	closedNow := t.closed.Load()
	t.mu.Unlock()
	if closedNow {
		// Close won the race between our closed check and completion; Close
		// is guaranteed to tear the stream down via streamCancel and its
		// re-snapshot. Report failure, not success, on a closed transport.
		return ErrTransportClosed
	}
	if t.closed.Load() {
		// Best-effort window shrink, not a guarantee: Close may still
		// interleave before the store below. Ready() masks with !closed,
		// so the flag cannot observably lie.
		return ErrTransportClosed
	}
	// The stream may have died between the goroutine spawn and here; never
	// mark a dead single-use transport ready.
	select {
	case <-t.doneCh:
	default:
		t.ready.Store(true)
	}
	return nil
}

// abortHandshake rolls back a failed endpoint handshake: cancels the stream,
// closes and unregisters the body, and releases the connect slot.
func (t *HTTPTransport) abortHandshake(sseCancel context.CancelFunc, connectFailed func()) {
	sseCancel()
	t.mu.Lock()
	body := t.sseBody
	t.sseBody = nil
	t.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
	connectFailed()
}

// readEndpointWithTimeout bounds readSSEInitialEndpoint with ctx even though
// the underlying blocking read (readBoundedLine) cannot be interrupted: on
// ctx expiry the helper
// returns immediately and the caller closes the body, which unblocks the
// helper goroutine (result channel is buffered so it never leaks).
func (t *HTTPTransport) readEndpointWithTimeout(ctx context.Context, reader *bufio.Reader, baseURL *url.URL) (string, error) {
	type endpointResult struct {
		endpoint string
		err      error
	}
	ch := make(chan endpointResult, 1)
	go func() {
		t.trackGoroutineStart()
		endpoint, err := readSSEInitialEndpoint(ctx, reader, baseURL, t.cfg.Logger)
		t.trackGoroutineEnd()
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

		line, err := readBoundedLine(reader)
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
			// Lenient fallback for non-compliant servers that send the raw
			// endpoint URL instead of the spec's JSON object. Logged at Warn
			// because it yields an unverifiable endpoint; compliant servers
			// never hit this path.
			logger.Warn("non-JSON SSE endpoint event, treating raw data as endpoint", "data", data)
			parsed.Endpoint = data
		}

		if parsed.Endpoint == "" {
			continue
		}

		endpointURL := parsed.Endpoint
		if !strings.HasPrefix(endpointURL, "http") {
			// Resolve relative endpoints, then fall through to the SAME
			// host validation as absolute ones: without it a crafted value
			// like "@evil.com/x" would become userinfo and hijack the POST
			// target (exfiltrating auth headers).
			endpointURL = baseURL.Scheme + "://" + baseURL.Host + endpointURL
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

func (t *HTTPTransport) sendHTTPPost(ctx context.Context, endpoint string, reqBytes []byte) (*http.Response, error) {
	return util.DoWithRetryResp(ctx, 2, func() (*http.Response, error) {
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBytes))
		if reqErr != nil {
			return nil, fmt.Errorf("creating POST request: %w", reqErr)
		}
		t.setHeaders(httpReq)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, doErr := t.httpClient.Do(httpReq)
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

// SendRequest issues a JSON-RPC request over the SSE session's POST endpoint
// and blocks until the response arrives, ctx expires, or RequestTimeout
// elapses. Requires a live stream (ErrTransportNotReady otherwise).
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

	if t.closed.Load() {
		// Close raced between the ready check and registration above: its
		// failPending has already fired, so no response can ever arrive.
		// Fail fast instead of waiting out RequestTimeout.
		return nil, ErrTransportClosed
	}

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

	httpResp, err := t.sendHTTPPost(postCtx, endpoint, reqBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("POST to MCP endpoint failed: %w", err)
	}
	return t.awaitResponse(postCtx, httpResp, id, ch)
}

// awaitResponse interprets the POST reply: 202 means the response arrives
// asynchronously over SSE (wait on ch); otherwise the body must carry the
// JSON-RPC response for the given request ID.
func (t *HTTPTransport) awaitResponse(ctx context.Context, httpResp *http.Response, id int64, ch chan *Response) (*Response, error) {
	defer func() { _ = httpResp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading MCP response: %w", err)
	}

	if httpResp.StatusCode == http.StatusAccepted {
		return t.waitForJSONRPCResponse(ctx, ch)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP endpoint returned status %d: %s", httpResp.StatusCode, string(body))
	}

	var rpcResp Response
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshaling MCP response: %w", err)
	}

	if rpcResp.ID == nil {
		if rpcResp.Error != nil {
			// Hostile/broken server: an ID-less error reply must surface as
			// an error, not as a successful response with no result.
			return nil, rpcResp.Error
		}
		return &rpcResp, nil
	}

	respID, ok := rpcResp.ID.(float64)
	if ok && int64(respID) == id {
		if rpcResp.Error != nil {
			return &rpcResp, rpcResp.Error
		}
		return &rpcResp, nil
	}

	return t.waitForJSONRPCResponse(ctx, ch)
}

func (t *HTTPTransport) waitForJSONRPCResponse(ctx context.Context, ch chan *Response) (*Response, error) {
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SendNotification posts a fire-and-forget JSON-RPC notification to the
// session endpoint, bounded by RequestTimeout. Requires a live stream.
// ctx must be non-nil.
func (t *HTTPTransport) SendNotification(ctx context.Context, method string, params any) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}
	if !t.ready.Load() {
		return ErrTransportNotReady
	}
	return t.postNotification(ctx, method, params)
}

// postNotification sends a notification POST without the readiness gate so
// the keepalive loop can probe (and recover) an unhealthy transport. Fails
// on any 4xx/5xx response — a rejected POST means the session is broken.
func (t *HTTPTransport) postNotification(ctx context.Context, method string, params any) error {
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
	// RequestTimeout. ctx must be non-nil; a nil ctx is a caller bug and is
	// not repaired here.
	reqCtx, cancel := context.WithTimeout(ctx, t.cfg.RequestTimeout)
	defer cancel()

	resp, err := t.sendHTTPPost(reqCtx, endpoint, reqBytes)
	if err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 400 {
		return fmt.Errorf("notification rejected with status %d", resp.StatusCode)
	}
	return nil
}

// Ready reports whether the transport can currently carry requests: the SSE
// stream is live, the session endpoint is known, and no keepalive failure
// has marked it unhealthy. Cleared by Close.
func (t *HTTPTransport) Ready() bool {
	return t.ready.Load() && !t.closed.Load()
}

// failPending fails all in-flight requests with the given error message and
// empties the pending map. Caller-safe from both Close and sseReadLoop
// cleanup: pending channels are buffered(1) so sends never block.
func (t *HTTPTransport) failPending(message string) {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	for id, ch := range t.pending {
		ch <- &Response{
			JSONRPC: JSONRPCVersion2,
			Error:   &Error{Code: ErrCodeInternal, Message: message},
		}
		delete(t.pending, id)
	}
}

// Close tears down the transport exactly once: cancels the SSE stream and
// its lifetime context, fails all pending requests, and waits up to 5s for
// the read loop to exit. Idempotent; always returns nil.
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

		t.failPending("transport closed")

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
		case <-t.doneCh:
			// Stream died (not via Close): stop pinging a dead session.
			return
		case <-ticker.C:
			// Ping regardless of readiness, bypassing the ready gate via
			// postNotification: after a transient blip the next successful
			// ping restores readiness, so one failed POST cannot brick the
			// client for its lifetime.
			pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := t.postNotification(pingCtx, "ping", nil); err != nil {
				cancel()
				if !t.closed.Load() {
					t.cfg.Logger.Warn("MCP keepalive ping failed", "err", err)
					t.ready.Store(false)
				}
				continue
			}
			cancel()
			// Restore readiness only if the stream is still live: a ping
			// completing exactly as the stream dies must not resurrect the
			// flag on a transport that can never serve again.
			select {
			case <-t.doneCh:
			default:
				if !t.closed.Load() {
					t.ready.Store(true)
				}
			}
		}
	}
}

// maxSSELineBytes bounds a single SSE line: a malicious server must not be
// able to OOM the gateway with one unterminated line.
const maxSSELineBytes = 1 << 20 // 1 MiB

// readBoundedLine reads one '\n'-terminated line, failing if it exceeds
// maxSSELineBytes (ReadString would otherwise grow unbounded).
func readBoundedLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxSSELineBytes {
			return "", fmt.Errorf("SSE line exceeds %d bytes", maxSSELineBytes)
		}
		switch err {
		case nil:
			return string(line), nil
		case bufio.ErrBufferFull:
			// Line longer than the read buffer; keep accumulating.
		default:
			return "", err
		}
	}
}

func readMultiLineEvent(reader *bufio.Reader) string {
	for {
		nextLine, readErr := readBoundedLine(reader)
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
		// The stream died without Close: fail in-flight requests now so
		// they get a transport-level error instead of waiting out their
		// full RequestTimeout.
		t.failPending("SSE stream lost")
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

		line, err := readBoundedLine(reader)
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
		case <-t.doneCh:
			// Stream died (not via Close): stop dispatching events.
			return
		case event := <-t.eventCh:
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
				// Only mark ready if the stream is still live and the
				// transport is not closed (select may pick a ready event
				// case even when closeCh is also ready).
				select {
				case <-t.doneCh:
				default:
					if !t.closed.Load() {
						t.ready.Store(true)
					}
				}
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

// SessionEndpoint returns the dynamic POST endpoint negotiated during the
// SSE handshake, or "" before the handshake completes.
func (t *HTTPTransport) SessionEndpoint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionEndpoint
}

// parseAndValidateEndpoint validates the dynamic MCP endpoint event: the
// endpoint must share the SSE connection's host. Relative endpoints are
// resolved against the configured origin, matching the handshake path.
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
	t.mu.Lock()
	baseHost := t.baseHost
	t.mu.Unlock()
	endpointURL := parsed.Endpoint
	if !strings.HasPrefix(endpointURL, "http") {
		scheme := "http"
		if u, parseErr := url.Parse(t.cfg.URL); parseErr == nil && u.Scheme != "" {
			scheme = u.Scheme
		}
		endpointURL = scheme + "://" + baseHost + endpointURL
	}
	epURL, err := url.Parse(endpointURL)
	if err != nil {
		return "", false
	}
	if epURL.Host != baseHost {
		gotHost := epURL.Host
		t.cfg.Logger.Warn("MCP server sent dynamic endpoint with unexpected host, rejecting",
			"expected_host", baseHost, "got_host", gotHost, "endpoint", parsed.Endpoint)
		return "", false
	}
	return endpointURL, true
}
