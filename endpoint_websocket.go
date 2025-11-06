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

const (
	ErrorCategoryNetwork WebSocketErrorCategory = iota
	ErrorCategoryAuth
	ErrorCategoryProtocol
	ErrorCategoryTimeout
	ErrorCategoryUnknown
)

// WebSocketStateCallback is called when the connection state changes
type WebSocketStateCallback func(oldState, newState WebSocketState, err error)

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
//	node, err := gomavlib.NewNode(gomavlib.NodeConf{
//	    Endpoints: []gomavlib.EndpointConf{
//	        gomavlib.EndpointWebSocket{
//	            URL: "ws://localhost:8080/mavlink",
//	            Headers: map[string]string{
//	                "Authorization": "Bearer token",
//	            },
//	            OnStateChange: func(old, new WebSocketState, err error) {
//	                log.Printf("WebSocket state: %s -> %s", old, new)
//	            },
//	        },
//	    },
//	    ...
//	})
type EndpointWebSocket struct {
	// WebSocket URL to connect to (e.g., "ws://localhost:8080/mavlink")
	URL string

	// Optional HTTP headers to send during WebSocket handshake
	Headers map[string]string

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
}

func (conf EndpointWebSocket) init(node *Node) (Endpoint, error) {
	// Validate URL
	if _, err := url.Parse(conf.URL); err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	// Set defaults
	if conf.InitialRetryPeriod == 0 {
		conf.InitialRetryPeriod = 1 * time.Second
	}
	if conf.MaxRetryPeriod == 0 {
		conf.MaxRetryPeriod = 30 * time.Second
	}
	if conf.BackoffMultiplier == 0 {
		conf.BackoffMultiplier = 1.5
	}
	if conf.HandshakeTimeout == 0 {
		conf.HandshakeTimeout = 5 * time.Second
	}
	if conf.PingPeriod == 0 {
		conf.PingPeriod = 30 * time.Second
	}
	if conf.PongWait == 0 {
		conf.PongWait = 120 * time.Second
	}
	if conf.MaxConsecutiveErrors == 0 {
		conf.MaxConsecutiveErrors = 10
	}
	if conf.CircuitBreakerTimeout == 0 {
		conf.CircuitBreakerTimeout = 5 * time.Minute
	}

	e := &endpointWebSocket{
		node: node,
		conf: conf,
	}
	err := e.initialize()
	return e, err
}

type endpointWebSocket struct {
	node *Node
	conf EndpointWebSocket

	ctx       context.Context
	cancel    context.CancelFunc
	terminate chan struct{}
	mu        sync.Mutex

	// State management
	state WebSocketState

	// Reconnection state
	reconnectAttempts    int32
	consecutiveErrors    int32
	currentRetryPeriod   time.Duration
	lastConnectTime      time.Time
	lastErrorTime        time.Time
	circuitBreakerOpenAt time.Time
}

func (e *endpointWebSocket) initialize() error {
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.terminate = make(chan struct{})
	e.currentRetryPeriod = e.conf.InitialRetryPeriod
	e.setState(WSStateDisconnected, nil)
	return nil
}

func (e *endpointWebSocket) close() {
	e.cancel()
	close(e.terminate)
}

func (e *endpointWebSocket) isEndpoint() {}

func (e *endpointWebSocket) Conf() EndpointConf {
	return e.conf
}

func (e *endpointWebSocket) oneChannelAtAtime() bool {
	// Return true for automatic reconnection behavior
	// When the connection drops, provide() will be called again
	return true
}

func (e *endpointWebSocket) setState(newState WebSocketState, err error) {
	e.mu.Lock()
	oldState := e.state
	e.state = newState
	callback := e.conf.OnStateChange
	e.mu.Unlock()

	if callback != nil && oldState != newState {
		callback(oldState, newState, err)
	}
}

func (e *endpointWebSocket) isCircuitBreakerOpen() bool {
	if e.circuitBreakerOpenAt.IsZero() {
		return false
	}
	return time.Since(e.circuitBreakerOpenAt) < e.conf.CircuitBreakerTimeout
}

func (e *endpointWebSocket) openCircuitBreaker() {
	e.circuitBreakerOpenAt = time.Now()
}

func (e *endpointWebSocket) closeCircuitBreaker() {
	e.circuitBreakerOpenAt = time.Time{}
}

func (e *endpointWebSocket) categorizeError(err error) WebSocketErrorCategory {
	if err == nil {
		return ErrorCategoryUnknown
	}

	errStr := err.Error()

	// Network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorCategoryTimeout
		}
		return ErrorCategoryNetwork
	}

	// WebSocket close errors
	if websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure,
		websocket.CloseNoStatusReceived) {
		return ErrorCategoryNetwork
	}

	// Authentication errors
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "403") ||
		strings.Contains(errStr, "unauthorized") || strings.Contains(errStr, "forbidden") {
		return ErrorCategoryAuth
	}

	// Protocol errors
	if strings.Contains(errStr, "protocol") || strings.Contains(errStr, "handshake") {
		return ErrorCategoryProtocol
	}

	// Timeout errors
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
		return ErrorCategoryTimeout
	}

	return ErrorCategoryUnknown
}

func (e *endpointWebSocket) shouldRetry(err error) (bool, time.Duration) {
	category := e.categorizeError(err)

	// Don't retry auth errors
	if category == ErrorCategoryAuth {
		return false, 0
	}

	// Check circuit breaker
	if e.isCircuitBreakerOpen() {
		return false, e.conf.CircuitBreakerTimeout - time.Since(e.circuitBreakerOpenAt)
	}

	// Check consecutive errors
	consecutiveErrors := atomic.LoadInt32(&e.consecutiveErrors)
	if int(consecutiveErrors) >= e.conf.MaxConsecutiveErrors {
		e.openCircuitBreaker()
		return false, e.conf.CircuitBreakerTimeout
	}

	// Check max reconnect attempts
	attempts := atomic.LoadInt32(&e.reconnectAttempts)
	if e.conf.MaxReconnectAttempts > 0 && int(attempts) >= e.conf.MaxReconnectAttempts {
		return false, 0
	}

	return true, e.currentRetryPeriod
}

func (e *endpointWebSocket) provide() (string, io.ReadWriteCloser, error) {
	for {
		select {
		case <-e.terminate:
			return "", nil, errTerminated
		default:
		}

		// Check if we should attempt connection
		attempts := atomic.LoadInt32(&e.reconnectAttempts)
		if attempts > 0 {
			e.setState(WSStateReconnecting, nil)

			// Wait for backoff period
			select {
			case <-time.After(e.currentRetryPeriod):
			case <-e.terminate:
				return "", nil, errTerminated
			}

			// Increase backoff period
			nextPeriod := time.Duration(float64(e.currentRetryPeriod) * e.conf.BackoffMultiplier)
			e.currentRetryPeriod = min(nextPeriod, e.conf.MaxRetryPeriod)
		} else {
			e.setState(WSStateConnecting, nil)
		}

		atomic.AddInt32(&e.reconnectAttempts, 1)

		// Attempt connection
		conn, err := e.connect()
		if err != nil {
			e.lastErrorTime = time.Now()
			atomic.AddInt32(&e.consecutiveErrors, 1)

			shouldRetry, retryAfter := e.shouldRetry(err)
			if !shouldRetry {
				e.setState(WSStateDisconnected, err)
				return "", nil, fmt.Errorf("connection failed and retry not allowed: %w", err)
			}

			e.currentRetryPeriod = retryAfter
			continue // Retry with backoff
		}

		// Connection successful - reset error counters
		atomic.StoreInt32(&e.consecutiveErrors, 0)
		e.currentRetryPeriod = e.conf.InitialRetryPeriod
		e.lastConnectTime = time.Now()
		e.closeCircuitBreaker()
		e.setState(WSStateConnected, nil)

		// Create durable adapter
		rwc := &webSocketConn{
			conn:       conn,
			ctx:        e.ctx,
			readCh:     make(chan []byte, 100),
			pingPeriod: e.conf.PingPeriod,
			pongWait:   e.conf.PongWait,
		}

		// Start background goroutines
		go rwc.reader()
		go rwc.pinger()

		label := e.conf.Label
		if label == "" {
			label = "websocket"
		}

		// Use removeCloser to prevent gomavlib from closing when channel closes
		// The connection will be managed by the reconnection loop
		return label, &removeCloser{rwc}, nil
	}
}

func (e *endpointWebSocket) connect() (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: e.conf.HandshakeTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}

	headers := make(map[string][]string)
	for k, v := range e.conf.Headers {
		headers[k] = []string{v}
	}

	conn, _, err := dialer.DialContext(e.ctx, e.conf.URL, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	return conn, nil
}

// GetState returns the current connection state
func (e *endpointWebSocket) GetState() WebSocketState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// IsHealthy returns true if the connection is currently healthy
func (e *endpointWebSocket) IsHealthy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == WSStateConnected
}

// GetStats returns connection statistics
func (e *endpointWebSocket) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"state":                e.GetState().String(),
		"reconnect_attempts":   atomic.LoadInt32(&e.reconnectAttempts),
		"consecutive_errors":   atomic.LoadInt32(&e.consecutiveErrors),
		"last_connect_time":    e.lastConnectTime,
		"last_error_time":      e.lastErrorTime,
		"circuit_breaker_open": e.isCircuitBreakerOpen(),
		"current_retry_period": e.currentRetryPeriod,
	}
}

// webSocketConn adapts a gorilla/websocket.Conn to io.ReadWriteCloser with durability features
type webSocketConn struct {
	conn       *websocket.Conn
	ctx        context.Context
	readCh     chan []byte
	pingPeriod time.Duration
	pongWait   time.Duration
	mu         sync.Mutex
	closed     bool

	// Statistics
	bytesRead       int64
	bytesWritten    int64
	messagesRead    int64
	messagesWritten int64
}

// reader continuously reads from WebSocket and forwards to readCh
func (w *webSocketConn) reader() {
	defer close(w.readCh)

	// Set pong handler
	w.conn.SetReadDeadline(time.Now().Add(w.pongWait))
	w.conn.SetPongHandler(func(string) error {
		w.conn.SetReadDeadline(time.Now().Add(w.pongWait))
		return nil
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
		atomic.AddInt64(&w.messagesRead, 1)
		atomic.AddInt64(&w.bytesRead, int64(len(data)))

		// Only forward binary messages
		if messageType != websocket.BinaryMessage {
			continue
		}

		select {
		case w.readCh <- data:
		case <-w.ctx.Done():
			return
		default:
			// Drop if buffer is full
		}
	}
}

// pinger sends ping messages periodically
func (w *webSocketConn) pinger() {
	ticker := time.NewTicker(w.pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.mu.Lock()
			if w.closed {
				w.mu.Unlock()
				return
			}
			err := w.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
			w.mu.Unlock()

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
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, io.ErrClosedPipe
	}

	err = w.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}

	// Update statistics
	atomic.AddInt64(&w.messagesWritten, 1)
	atomic.AddInt64(&w.bytesWritten, int64(len(p)))

	return len(p), nil
}

func (w *webSocketConn) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true

	// Send close message
	w.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)

	return w.conn.Close()
}

// GetStats returns connection statistics
func (w *webSocketConn) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"bytes_read":       atomic.LoadInt64(&w.bytesRead),
		"bytes_written":    atomic.LoadInt64(&w.bytesWritten),
		"messages_read":    atomic.LoadInt64(&w.messagesRead),
		"messages_written": atomic.LoadInt64(&w.messagesWritten),
	}
}
