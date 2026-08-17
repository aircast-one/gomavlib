package gomavlib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketState represents the current state of the WebSocket connection
type WebSocketState int32

// WebSocket connection states.
const (
	WSStateDisconnected WebSocketState = iota
	WSStateConnecting
	WSStateConnected
	WSStateReconnecting
)

func (s WebSocketState) String() string {
	switch s {
	case WSStateDisconnected:
		return "disconnected"
	case WSStateConnecting:
		return "connecting"
	case WSStateConnected:
		return "connected"
	case WSStateReconnecting:
		return "reconnecting"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// WebSocketErrorCategory represents the type of error that occurred
type WebSocketErrorCategory int

// WebSocket error categories used to decide whether a failure is retryable.
const (
	ErrorCategoryNetwork WebSocketErrorCategory = iota
	ErrorCategoryAuth
	ErrorCategoryProtocol
	ErrorCategoryTimeout
	ErrorCategoryUnknown
)

// WebSocketStateCallback is called when the connection state changes
type WebSocketStateCallback func(oldState, newState WebSocketState, err error)

var _ Endpoint = (*EndpointWebSocket)(nil)

// EndpointWebSocket sets up a durable WebSocket endpoint with automatic reconnection.
// This is designed for persistent WebSocket connections used in MAVLink-over-WebSocket scenarios.
//
// Features:
// - Automatic reconnection with exponential backoff
// - Connection health monitoring with ping/pong
// - Circuit breaker pattern to prevent connection storms
// - Error categorization and intelligent retry strategies
// - Connection state callbacks for monitoring
//
// Example:
//
//	node := &gomavlib.Node{
//	    Endpoints: []gomavlib.Endpoint{
//	        &gomavlib.EndpointWebSocket{
//	            URL: "ws://localhost:8080/mavlink",
//	            Headers: map[string]string{
//	                "Authorization": "Bearer token",
//	            },
//	            OnStateChange: func(old, new WebSocketState, err error) {
//	                log.Printf("WebSocket state: %s -> %s", old, new)
//	            },
//	        },
//	    },
//	}
//	err := node.Initialize()
type EndpointWebSocket struct {
	// WebSocket URL to connect to (e.g., "ws://localhost:8080/mavlink")
	URL string

	// Optional HTTP headers to send during WebSocket handshake.
	// For static headers that don't change between reconnections.
	Headers map[string]string

	// Optional callback that provides headers dynamically on each connection attempt.
	// When set, this is called instead of using the static Headers map.
	// This is useful for authentication tokens that may expire and need refreshing
	// between reconnection attempts.
	//
	// If the provider returns a non-nil error, the connection attempt is skipped
	// entirely (no HTTP request is made) and the error is counted as a failure
	// for the circuit breaker. This prevents sending unauthenticated requests
	// when the token cannot be obtained.
	HeaderProvider func() (map[string]string, error)

	// Optional label for logging (defaults to "websocket")
	Label string

	// Reconnection configuration (optional, uses defaults if not set)
	InitialRetryPeriod   time.Duration // Default: 1s
	MaxRetryPeriod       time.Duration // Default: 30s
	BackoffMultiplier    float64       // Default: 1.5
	MaxReconnectAttempts int           // Default: 0 (unlimited)

	// Connection timeouts (optional, uses defaults if not set)
	HandshakeTimeout time.Duration // Default: 5s
	PingPeriod       time.Duration // Default: 30s
	PongWait         time.Duration // Default: 120s

	// Circuit breaker configuration
	MaxConsecutiveErrors  int           // Default: 10
	CircuitBreakerTimeout time.Duration // Default: 5 minutes

	// State change callback (optional)
	OnStateChange WebSocketStateCallback

	// NetDialContext specifies a custom dial function for creating TCP connections.
	// This is useful for routing through custom networks like Tailscale.
	// If nil, the default dialer is used.
	NetDialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	node      *Node
	ctx       context.Context
	cancel    context.CancelFunc
	terminate chan struct{}
	mu        sync.Mutex

	state WebSocketState

	retryState      *RetryState
	lastConnectTime time.Time
	lastErrorTime   time.Time
}

func (e *EndpointWebSocket) init(node *Node) error {
	if _, err := url.Parse(e.URL); err != nil {
		return fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	if e.InitialRetryPeriod == 0 {
		e.InitialRetryPeriod = 1 * time.Second
	}
	if e.MaxRetryPeriod == 0 {
		e.MaxRetryPeriod = 30 * time.Second
	}
	if e.BackoffMultiplier == 0 {
		e.BackoffMultiplier = 1.5
	}
	if e.HandshakeTimeout == 0 {
		e.HandshakeTimeout = 5 * time.Second
	}
	if e.PingPeriod == 0 {
		e.PingPeriod = 30 * time.Second
	}
	if e.PongWait == 0 {
		e.PongWait = 120 * time.Second
	}
	if e.MaxConsecutiveErrors == 0 {
		e.MaxConsecutiveErrors = 10
	}
	if e.CircuitBreakerTimeout == 0 {
		e.CircuitBreakerTimeout = 5 * time.Minute
	}

	e.node = node
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.terminate = make(chan struct{})
	e.retryState = NewRetryState(RetryPolicy{
		InitialRetryPeriod:    e.InitialRetryPeriod,
		MaxRetryPeriod:        e.MaxRetryPeriod,
		BackoffMultiplier:     e.BackoffMultiplier,
		MaxReconnectAttempts:  e.MaxReconnectAttempts,
		MaxConsecutiveErrors:  e.MaxConsecutiveErrors,
		CircuitBreakerTimeout: e.CircuitBreakerTimeout,
	})
	e.setState(WSStateDisconnected, nil)

	return nil
}

func (e *EndpointWebSocket) close() {
	e.cancel()
	close(e.terminate)
}

func (e *EndpointWebSocket) isEndpoint() {}

func (e *EndpointWebSocket) isDatagram() bool {
	return false
}

func (e *EndpointWebSocket) oneChannelAtAtime() bool {
	// Return true for automatic reconnection behavior
	// When the connection drops, provide() will be called again
	return true
}

func (e *EndpointWebSocket) setState(newState WebSocketState, err error) {
	e.mu.Lock()
	oldState := e.state
	e.state = newState
	callback := e.OnStateChange
	e.mu.Unlock()

	if callback != nil && oldState != newState {
		callback(oldState, newState, err)
	}
}

// Circuit breaker methods delegate to RetryState for consolidated retry logic

// categorizeError determines the error category for retry decisions.
//
// Error categorization strategy:
//  1. Type assertions (preferred): errors.As for net.Error, websocket.IsCloseError
//  2. String matching (fallback): For errors without typed wrappers
//
// String matching rationale:
// - Many HTTP/WebSocket errors wrap underlying errors as strings
// - HTTP status codes (401, 403) appear in error messages but not as typed errors
// - This is a pragmatic fallback when proper error types aren't available
// - Case-insensitive matching handles varying error message formats
//
// Known limitations:
// - String matching is locale-independent but message-dependent
// - Error message changes could affect categorization
// - We prioritize type assertions where possible to minimize string matching
func (e *EndpointWebSocket) categorizeError(err error) WebSocketErrorCategory {
	if err == nil {
		return ErrorCategoryUnknown
	}

	errStr := err.Error()

	// Network errors - prefer type assertion
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorCategoryTimeout
		}
		return ErrorCategoryNetwork
	}

	// WebSocket close errors - use library's typed check
	if websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure,
		websocket.CloseNoStatusReceived) {
		return ErrorCategoryNetwork
	}

	// --- String matching fallback for untyped errors ---
	// Note: These checks are necessary because HTTP errors from the WebSocket
	// handshake don't expose typed status codes in the error interface.

	// Authentication errors (HTTP 401/403 or header provider failure)
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "403") ||
		strings.Contains(errStr, "unauthorized") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "header provider failed") {
		return ErrorCategoryAuth
	}

	// Protocol errors
	if strings.Contains(errStr, "protocol") || strings.Contains(errStr, "handshake") {
		return ErrorCategoryProtocol
	}

	// Timeout errors (backup for non-net.Error timeouts)
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
		return ErrorCategoryTimeout
	}

	return ErrorCategoryUnknown
}

// shouldRetryError checks if an error is retryable based on its category.
// Circuit breaker and max attempts are handled by RetryState.RecordError().
func (e *EndpointWebSocket) shouldRetryError(err error) bool {
	category := e.categorizeError(err)

	// Auth errors are retryable only when a HeaderProvider is set,
	// because the provider may return fresh tokens on the next attempt.
	// Without a provider, auth errors require user intervention.
	if category == ErrorCategoryAuth {
		return e.HeaderProvider != nil
	}

	// Check if circuit breaker is open (managed by RetryState)
	if e.retryState.IsCircuitBreakerOpen() {
		return false
	}

	return true
}

func (e *EndpointWebSocket) provide() (string, io.ReadWriteCloser, error) {
	for {
		select {
		case <-e.terminate:
			return "", nil, errTerminated
		default:
		}

		// Check if we should wait before attempting (uses common retry logic)
		shouldWait, waitDuration := e.retryState.BeforeAttempt()
		if shouldWait {
			e.setState(WSStateReconnecting, nil)

			// Wait for backoff period
			select {
			case <-time.After(waitDuration):
			case <-e.terminate:
				return "", nil, errTerminated
			}
		} else {
			e.setState(WSStateConnecting, nil)
		}

		// Record that we're making an attempt
		e.retryState.RecordAttempt()

		// Attempt connection
		conn, err := e.connect()
		if err != nil {
			e.lastErrorTime = time.Now()

			// Record the error and check if we should continue retrying
			// Check if error is retryable (auth errors are not)
			if !e.shouldRetryError(err) {
				e.setState(WSStateDisconnected, err)
				<-e.terminate
				return "", nil, errTerminated
			}

			// Record the error - this handles circuit breaker and max attempts
			shouldContinue := e.retryState.RecordError()
			if !shouldContinue {
				e.setState(WSStateDisconnected, err)
				// Wait for termination - don't return error as gomavlib only expects errTerminated
				<-e.terminate
				return "", nil, errTerminated
			}

			continue // Retry with backoff
		}

		// Connection successful - reset error counters (also closes circuit breaker)
		e.retryState.RecordSuccess()
		e.lastConnectTime = time.Now()
		e.setState(WSStateConnected, nil)

		// Create durable adapter
		rwc := &webSocketConn{
			conn:       conn,
			ctx:        e.ctx,
			readCh:     make(chan []byte, 100),
			pingPeriod: e.PingPeriod,
			pongWait:   e.PongWait,
		}

		// Start background goroutines
		go rwc.reader()
		go rwc.pinger()

		label := e.Label
		if label == "" {
			label = "websocket"
		}

		// Use removeCloser to prevent gomavlib from closing when channel closes
		// The connection will be managed by the reconnection loop
		return label, &removeCloser{rwc}, nil
	}
}

func (e *EndpointWebSocket) connect() (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: e.HandshakeTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		NetDialContext:   e.NetDialContext,
	}

	// Use HeaderProvider for dynamic headers (e.g., refreshed auth tokens),
	// falling back to static Headers map.
	var sourceHeaders map[string]string
	if e.HeaderProvider != nil {
		var err error
		sourceHeaders, err = e.HeaderProvider()
		if err != nil {
			return nil, fmt.Errorf("header provider failed: %w", err)
		}
	} else {
		sourceHeaders = e.Headers
	}

	headers := make(map[string][]string)
	for k, v := range sourceHeaders {
		headers[k] = []string{v}
	}

	conn, res, err := dialer.DialContext(e.ctx, e.URL, headers)
	if err != nil {
		if res != nil {
			res.Body.Close()
		}
		return nil, fmt.Errorf("failed to connect to WebSocket: %w", err)
	}
	res.Body.Close()

	return conn, nil
}

// GetState returns the current connection state
func (e *EndpointWebSocket) GetState() WebSocketState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// IsHealthy returns true if the connection is currently healthy
func (e *EndpointWebSocket) IsHealthy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == WSStateConnected
}

// GetStats returns connection statistics
func (e *EndpointWebSocket) GetStats() map[string]any {
	stats := e.retryState.GetStats()
	stats["state"] = e.GetState().String()
	stats["last_connect_time"] = e.lastConnectTime
	stats["last_error_time"] = e.lastErrorTime
	// circuit_breaker_open is already included from retryState.GetStats()
	return stats
}

// wsWriteDeadline is the maximum time a data frame write can block before timing out.
// This prevents Write() from holding writeMu indefinitely on slow 4G networks,
// which would block the ping handler from sending pong responses (since the ping
// handler runs on the reader goroutine inside ReadMessage and previously needed
// the same mutex).
const wsWriteDeadline = 15 * time.Second

// webSocketConn adapts a gorilla/websocket.Conn to io.ReadWriteCloser with durability features.
//
// Concurrency model:
//   - writeMu serializes data frame writes (WriteMessage) only
//   - Control frames (ping/pong/close) use WriteControl which is goroutine-safe
//     in gorilla/websocket without external synchronization
//   - closed is atomic to allow lock-free checks from the ping handler
//     (which runs on the reader goroutine and must never block on writeMu)
type webSocketConn struct {
	conn       *websocket.Conn
	ctx        context.Context
	readCh     chan []byte
	pingPeriod time.Duration
	pongWait   time.Duration
	writeMu    sync.Mutex // serializes data frame WriteMessage calls only
	closed     atomic.Bool

	bytesRead       atomic.Int64
	bytesWritten    atomic.Int64
	messagesRead    atomic.Int64
	messagesWritten atomic.Int64
	messagesDropped atomic.Int64
}

// reader continuously reads from WebSocket and forwards to readCh
func (w *webSocketConn) reader() {
	defer close(w.readCh)

	// Set ping handler - respond to server pings with pongs.
	// WriteControl is goroutine-safe in gorilla/websocket, so no external mutex
	// is needed. This is critical: the ping handler runs inside ReadMessage() on
	// the reader goroutine. If it blocked on writeMu (held by a slow Write()),
	// the reader would freeze and no pongs would be sent, causing the server to
	// close the connection after its pongWait timeout.
	w.conn.SetPingHandler(func(message string) error {
		if w.closed.Load() {
			return nil
		}
		return w.conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(10*time.Second))
	})

	w.conn.SetReadDeadline(time.Now().Add(w.pongWait)) //nolint:errcheck
	w.conn.SetPongHandler(func(string) error {
		return w.conn.SetReadDeadline(time.Now().Add(w.pongWait))
	})

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		messageType, data, err := w.conn.ReadMessage()
		if err != nil {
			// Connection error - exit and trigger reconnection
			return
		}

		// Update statistics
		w.messagesRead.Add(1)
		w.bytesRead.Add(int64(len(data)))

		// Only forward binary messages
		if messageType != websocket.BinaryMessage {
			continue
		}

		select {
		case w.readCh <- data:
		case <-w.ctx.Done():
			return
		default:
			w.messagesDropped.Add(1)
		}
	}
}

// pinger sends ping messages periodically.
// WriteControl is goroutine-safe, so no external mutex is needed.
func (w *webSocketConn) pinger() {
	ticker := time.NewTicker(w.pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if w.closed.Load() {
				return
			}
			err := w.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
			if err != nil {
				// Ping failed - connection is dead
				return
			}
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *webSocketConn) Read(p []byte) (n int, err error) {
	select {
	case data, ok := <-w.readCh:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, data), nil
	case <-w.ctx.Done():
		return 0, io.EOF
	}
}

func (w *webSocketConn) Write(p []byte) (n int, err error) {
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	// Set write deadline to prevent blocking indefinitely on congested 4G networks.
	// Without this, a blocked WriteMessage holds writeMu forever. While writeMu no
	// longer blocks pong responses (control frames bypass it), a stuck write still
	// wastes resources and delays reconnection.
	err = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	if err != nil {
		return 0, err
	}

	err = w.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}

	// Update statistics
	w.messagesWritten.Add(1)
	w.bytesWritten.Add(int64(len(p)))

	return len(p), nil
}

func (w *webSocketConn) Close() error {
	if w.closed.Swap(true) {
		return nil // already closed
	}

	// Send close control frame (WriteControl is goroutine-safe)
	w.conn.WriteControl( //nolint:errcheck
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)

	// Wait for any in-progress data write to finish before closing the TCP conn
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	return w.conn.Close()
}

// GetStats returns connection statistics
func (w *webSocketConn) GetStats() map[string]any {
	return map[string]any{
		"bytes_read":       w.bytesRead.Load(),
		"bytes_written":    w.bytesWritten.Load(),
		"messages_read":    w.messagesRead.Load(),
		"messages_written": w.messagesWritten.Load(),
		"messages_dropped": w.messagesDropped.Load(),
	}
}
