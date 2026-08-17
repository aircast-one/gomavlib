package gomavlib

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// WebTransportLogger interface allows custom logging implementations.
// If not set, a default stderr logger is used.
type WebTransportLogger interface {
	Printf(format string, v ...any)
}

// defaultWTLogger management with thread-safe access
var (
	wtLoggerMu       sync.RWMutex
	wtLoggerInitOnce sync.Once
	wtLogger         WebTransportLogger
)

// getDefaultWTLogger returns the current WebTransport logger with lazy initialization.
// Thread-safe for concurrent access.
func getDefaultWTLogger() WebTransportLogger {
	// Lazy initialization on first access
	wtLoggerInitOnce.Do(func() {
		wtLogger = log.New(os.Stderr, "[gomavlib-webtransport] ", log.LstdFlags|log.Lmicroseconds)
	})

	wtLoggerMu.RLock()
	defer wtLoggerMu.RUnlock()
	return wtLogger
}

// SetWebTransportLogger sets the default logger for all WebTransport endpoints.
// Pass nil to disable logging. This is a package-level setting.
// Thread-safe for concurrent access.
func SetWebTransportLogger(logger WebTransportLogger) {
	// Ensure initialization has happened before we try to modify
	wtLoggerInitOnce.Do(func() {
		wtLogger = log.New(os.Stderr, "[gomavlib-webtransport] ", log.LstdFlags|log.Lmicroseconds)
	})

	wtLoggerMu.Lock()
	defer wtLoggerMu.Unlock()

	if logger == nil {
		wtLogger = discardLogger{}
	} else {
		wtLogger = logger
	}
}

// discardLogger discards all log messages
type discardLogger struct{}

func (discardLogger) Printf(_ string, _ ...any) {}

// Default QUIC configuration values
const (
	defaultMaxIdleTimeout        = 120 * time.Second
	defaultKeepAlivePeriod       = 30 * time.Second
	defaultMaxIncomingStreams    = 100
	defaultMaxIncomingUniStreams = 100
)

// ErrDatagramTruncated is returned when a received datagram exceeds the buffer size
var ErrDatagramTruncated = errors.New("datagram truncated: buffer too small")

var _ Endpoint = (*EndpointWebTransport)(nil)

// EndpointWebTransport sets up a WebTransport endpoint with QUIC connection migration.
// This endpoint survives IP address changes (e.g., cellular handoffs) without losing
// the connection, as QUIC uses connection IDs rather than IP:port tuples.
//
// Features:
// - Connection migration across IP changes (QUIC RFC 9000)
// - Zero-RTT reconnection when supported
// - Both reliable streams and unreliable datagrams
// - Automatic reconnection with exponential backoff
//
// Example:
//
//	node := &gomavlib.Node{
//	    Endpoints: []gomavlib.Endpoint{
//	        &gomavlib.EndpointWebTransport{
//	            URL: "https://server.example.com:443/mavlink",
//	            Headers: map[string]string{
//	                "Authorization": "Bearer token",
//	            },
//	            UseDatagrams: true, // Use unreliable datagrams for lower latency
//	        },
//	    },
//	}
//	err := node.Initialize()
type EndpointWebTransport struct {
	// WebTransport URL to connect to (must be https://)
	URL string

	// Optional HTTP headers for the WebTransport handshake.
	// For static headers that don't change between reconnections.
	Headers map[string]string

	// Optional callback that provides headers dynamically on each connection attempt.
	// When set, this is called instead of using the static Headers map.
	// This is useful for authentication tokens that may expire and need refreshing
	// between reconnection attempts.
	//
	// If the provider returns a non-nil error, the connection attempt is skipped
	// entirely (no HTTP request is made) and the error is counted as a failure
	// for the circuit breaker.
	HeaderProvider func() (map[string]string, error)

	// Optional label for logging (defaults to "webtransport")
	Label string

	// UseDatagrams enables unreliable datagram mode (lower latency, may drop)
	// If false, uses reliable bidirectional streams
	UseDatagrams bool

	// Reconnection configuration
	InitialRetryPeriod   time.Duration // Default: 1s
	MaxRetryPeriod       time.Duration // Default: 30s
	BackoffMultiplier    float64       // Default: 1.5
	MaxReconnectAttempts int           // Default: 0 (unlimited)

	// TLS configuration (optional)
	TLSConfig *tls.Config

	// QUIC configuration for connection migration (optional)
	QUICConfig *quic.Config

	// DialAddr is a custom function for dialing the QUIC connection.
	// This can be used to route connections through Tailscale or other custom transports.
	// If nil, the default quic.DialAddrEarly will be used.
	DialAddr func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error)

	// State change callback (optional)
	OnStateChange ConnectionStateCallback

	// Logger for WebTransport debugging (optional).
	// If nil, uses the package-level default logger.
	// Set to a custom logger or use SetWebTransportLogger(nil) to disable logging.
	Logger WebTransportLogger

	node      *Node
	ctx       context.Context
	cancel    context.CancelFunc
	terminate chan struct{}
	mu        sync.Mutex

	logger WebTransportLogger

	state   ConnectionState
	session *webtransport.Session
	dialer  *webtransport.Dialer

	retryState      *RetryState
	lastConnectTime time.Time
}

func (e *EndpointWebTransport) init(node *Node) error {
	if e.URL == "" {
		return errors.New("WebTransport URL is required")
	}

	parsedURL, err := url.Parse(e.URL)
	if err != nil {
		return fmt.Errorf("invalid WebTransport URL: %w", err)
	}
	if parsedURL.Scheme != "https" {
		return errors.New("WebTransport URL must use https:// scheme")
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

	if e.QUICConfig == nil {
		e.QUICConfig = &quic.Config{
			MaxIdleTimeout:                   defaultMaxIdleTimeout,
			KeepAlivePeriod:                  defaultKeepAlivePeriod,
			EnableDatagrams:                  e.UseDatagrams,
			EnableStreamResetPartialDelivery: true,
			Allow0RTT:                        true,
			MaxIncomingStreams:               defaultMaxIncomingStreams,
			MaxIncomingUniStreams:            defaultMaxIncomingUniStreams,
		}
	}

	e.node = node
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.terminate = make(chan struct{})

	if e.Logger != nil {
		e.logger = e.Logger
	} else {
		e.logger = getDefaultWTLogger()
	}

	e.retryState = NewRetryState(RetryPolicy{
		InitialRetryPeriod:   e.InitialRetryPeriod,
		MaxRetryPeriod:       e.MaxRetryPeriod,
		BackoffMultiplier:    e.BackoffMultiplier,
		MaxReconnectAttempts: e.MaxReconnectAttempts,
	})
	e.setState(ConnStateDisconnected, nil)

	e.dialer = &webtransport.Dialer{
		TLSClientConfig: e.TLSConfig,
		QUICConfig:      e.QUICConfig,
		DialAddr:        e.DialAddr,
	}

	return nil
}

func (e *EndpointWebTransport) isDatagram() bool {
	return false
}

func (e *EndpointWebTransport) close() {
	e.cancel()
	close(e.terminate)

	e.mu.Lock()
	if e.session != nil {
		e.session.CloseWithError(0, "endpoint closed") //nolint:errcheck
	}
	e.mu.Unlock()
}

func (e *EndpointWebTransport) isEndpoint() {}

func (e *EndpointWebTransport) oneChannelAtAtime() bool {
	return true
}

func (e *EndpointWebTransport) setState(newState ConnectionState, err error) {
	e.mu.Lock()
	oldState := e.state
	e.state = newState
	callback := e.OnStateChange
	e.mu.Unlock()

	if callback != nil && oldState != newState {
		callback(oldState, newState, err)
	}
}

func (e *EndpointWebTransport) provide() (string, io.ReadWriteCloser, error) {
	for {
		select {
		case <-e.terminate:
			return "", nil, errTerminated
		default:
		}

		// Check if we should wait before attempting (uses common retry logic)
		shouldWait, waitDuration := e.retryState.BeforeAttempt()
		if shouldWait {
			e.setState(ConnStateReconnecting, nil)

			select {
			case <-time.After(waitDuration):
			case <-e.terminate:
				return "", nil, errTerminated
			}
		} else {
			e.setState(ConnStateConnecting, nil)
		}

		// Record that we're making an attempt
		e.retryState.RecordAttempt()
		attempts := e.retryState.ReconnectAttempts()

		// Attempt connection
		rwc, err := e.connect()
		if err != nil {
			// Record the error and check if we should continue retrying
			shouldContinue := e.retryState.RecordError()
			stats := e.retryState.GetStats()
			e.logger.Printf("event=connect_failed url=%s attempt=%d consecutive_errors=%v retry_period_ms=%v error=%v",
				e.URL, attempts, stats["consecutive_errors"], stats["current_retry_period"].(time.Duration).Milliseconds(), err)

			if !shouldContinue {
				e.setState(ConnStateDisconnected, err)
				<-e.terminate
				return "", nil, errTerminated
			}

			continue
		}

		// Connection successful - reset error counters
		e.retryState.RecordSuccess()
		e.mu.Lock()
		e.lastConnectTime = time.Now()
		e.mu.Unlock()
		e.setState(ConnStateConnected, nil)

		e.logger.Printf("event=connect_success url=%s attempt=%d use_datagrams=%v",
			e.URL, attempts, e.UseDatagrams)

		label := e.Label
		if label == "" {
			label = "webtransport"
		}

		return label, &removeCloser{rwc}, nil
	}
}

func (e *EndpointWebTransport) connect() (io.ReadWriteCloser, error) {
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

	header := http.Header{}
	for k, v := range sourceHeaders {
		header.Set(k, v)
	}

	// Dial WebTransport session
	res, session, err := e.dialer.Dial(e.ctx, e.URL, header)
	if err != nil {
		if res != nil {
			res.Body.Close()
		}
		return nil, fmt.Errorf("failed to dial WebTransport: %w", err)
	}
	res.Body.Close()

	e.mu.Lock()
	e.session = session
	e.mu.Unlock()

	if e.UseDatagrams {
		// Use unreliable datagrams for MAVLink (lower latency)
		e.logger.Printf("event=datagram_conn_open url=%s use_datagrams=true", e.URL)
		return &webTransportDatagramConn{
			session:   session,
			ctx:       e.ctx,
			logger:    e.logger,
			startTime: time.Now(),
		}, nil
	}

	// Use reliable bidirectional stream
	stream, err := session.OpenStreamSync(e.ctx)
	if err != nil {
		session.CloseWithError(0, "failed to open stream") //nolint:errcheck
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	return &webTransportStreamConn{
		session: session,
		stream:  stream,
	}, nil
}

// GetState returns the current connection state
func (e *EndpointWebTransport) GetState() ConnectionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// WebTransportStats contains connection statistics
type WebTransportStats struct {
	State             string    `json:"state"`
	ReconnectAttempts int32     `json:"reconnect_attempts"`
	ConsecutiveErrors int32     `json:"consecutive_errors"`
	LastConnectTime   time.Time `json:"last_connect_time"`
}

// GetStats returns connection statistics
func (e *EndpointWebTransport) GetStats() WebTransportStats {
	e.mu.Lock()
	lastConnect := e.lastConnectTime
	e.mu.Unlock()

	return WebTransportStats{
		State:             e.GetState().String(),
		ReconnectAttempts: e.retryState.ReconnectAttempts(),
		ConsecutiveErrors: e.retryState.ConsecutiveErrors(),
		LastConnectTime:   lastConnect,
	}
}

// webTransportDatagramConn adapts WebTransport datagrams to io.ReadWriteCloser
type webTransportDatagramConn struct {
	session   *webtransport.Session
	ctx       context.Context
	logger    WebTransportLogger
	mu        sync.Mutex
	closed    bool
	startTime time.Time

	// Atomic counters for wide event logging
	datagramsRead    atomic.Int64
	datagramsWritten atomic.Int64
	readErrors       atomic.Int64
	writeErrors      atomic.Int64
	bytesRead        atomic.Int64
	bytesWritten     atomic.Int64
}

func (c *webTransportDatagramConn) Read(p []byte) (n int, err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.mu.Unlock()

	data, err := c.session.ReceiveDatagram(c.ctx)
	if err != nil {
		c.readErrors.Add(1)
		return 0, err
	}

	if len(data) > len(p) {
		// Return error only, no partial data. Returning partial MAVLink data would
		// cause parsing failures since MAVLink messages must be complete.
		// The caller should ensure buffer is sized appropriately (typically 280+ bytes
		// for MAVLink v2 max message size).
		c.readErrors.Add(1)
		return 0, ErrDatagramTruncated
	}

	bytesRead := copy(p, data)
	readCount := c.datagramsRead.Add(1)
	c.bytesRead.Add(int64(bytesRead))

	// Wide event: Log first 3 datagrams received and then periodically
	if readCount <= 3 || readCount%1000 == 0 {
		firstByte := "N/A"
		if bytesRead > 0 {
			firstByte = fmt.Sprintf("0x%02x", data[0])
		}
		c.logger.Printf("event=datagram_recv datagram_num=%d bytes=%d first_byte=%s total_read=%d total_bytes=%d",
			readCount, bytesRead, firstByte, readCount, c.bytesRead.Load())
	}

	return bytesRead, nil
}

func (c *webTransportDatagramConn) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	err = c.session.SendDatagram(p)
	if err != nil {
		c.writeErrors.Add(1)
		writeCount := c.datagramsWritten.Load()
		c.logger.Printf("event=datagram_send_error datagram_num=%d bytes=%d error=%v total_sent=%d total_errors=%d",
			writeCount+1, len(p), err, writeCount, c.writeErrors.Load())
		return 0, err
	}

	writeCount := c.datagramsWritten.Add(1)
	c.bytesWritten.Add(int64(len(p)))

	// Wide event: Log first 3 datagrams sent and then periodically
	if writeCount <= 3 || writeCount%1000 == 0 {
		firstByte := "N/A"
		if len(p) > 0 {
			firstByte = fmt.Sprintf("0x%02x", p[0])
		}
		c.logger.Printf("event=datagram_send datagram_num=%d bytes=%d first_byte=%s total_sent=%d total_bytes=%d",
			writeCount, len(p), firstByte, writeCount, c.bytesWritten.Load())
	}

	return len(p), nil
}

func (c *webTransportDatagramConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	// Wide event: Log connection lifecycle summary
	duration := time.Since(c.startTime)
	c.logger.Printf("event=datagram_conn_close duration_ms=%d datagrams_read=%d datagrams_written=%d "+
		"bytes_read=%d bytes_written=%d read_errors=%d write_errors=%d",
		duration.Milliseconds(),
		c.datagramsRead.Load(),
		c.datagramsWritten.Load(),
		c.bytesRead.Load(),
		c.bytesWritten.Load(),
		c.readErrors.Load(),
		c.writeErrors.Load())

	return c.session.CloseWithError(0, "closed")
}

// webTransportStreamConn adapts a WebTransport stream to io.ReadWriteCloser
type webTransportStreamConn struct {
	session *webtransport.Session
	stream  *webtransport.Stream
	mu      sync.Mutex
	closed  bool
}

func (c *webTransportStreamConn) Read(p []byte) (n int, err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.mu.Unlock()

	return c.stream.Read(p)
}

func (c *webTransportStreamConn) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	return c.stream.Write(p)
}

func (c *webTransportStreamConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	streamErr := c.stream.Close()
	sessionErr := c.session.CloseWithError(0, "closed")

	if streamErr != nil {
		return streamErr
	}
	return sessionErr
}
