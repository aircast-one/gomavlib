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
	Printf(format string, v ...interface{})
}

// defaultWTLogger management with thread-safe access
var (
	wtLoggerMu      sync.RWMutex
	wtLoggerInitOnce sync.Once
	wtLogger        WebTransportLogger
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

func (discardLogger) Printf(format string, v ...interface{}) {}

// Default QUIC configuration values
const (
	defaultMaxIdleTimeout        = 120 * time.Second
	defaultKeepAlivePeriod       = 30 * time.Second
	defaultMaxIncomingStreams    = 100
	defaultMaxIncomingUniStreams = 100
)

// ErrDatagramTruncated is returned when a received datagram exceeds the buffer size
var ErrDatagramTruncated = errors.New("datagram truncated: buffer too small")

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
//	node, err := gomavlib.NewNode(gomavlib.NodeConf{
//	    Endpoints: []gomavlib.EndpointConf{
//	        gomavlib.EndpointWebTransport{
//	            URL: "https://server.example.com:443/mavlink",
//	            Headers: map[string]string{
//	                "Authorization": "Bearer token",
//	            },
//	            UseDatagrams: true, // Use unreliable datagrams for lower latency
//	        },
//	    },
//	    ...
//	})
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
}

func (conf EndpointWebTransport) init(node *Node) (Endpoint, error) {
	if conf.URL == "" {
		return nil, errors.New("WebTransport URL is required")
	}

	// Validate URL scheme
	parsedURL, err := url.Parse(conf.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebTransport URL: %w", err)
	}
	if parsedURL.Scheme != "https" {
		return nil, errors.New("WebTransport URL must use https:// scheme")
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

	// Default QUIC config with connection migration enabled
	if conf.QUICConfig == nil {
		conf.QUICConfig = &quic.Config{
			MaxIdleTimeout:                   defaultMaxIdleTimeout,
			KeepAlivePeriod:                  defaultKeepAlivePeriod,
			EnableDatagrams:                  conf.UseDatagrams,
			EnableStreamResetPartialDelivery: true, // Required by webtransport-go v0.10.0
			Allow0RTT:                        true, // Enable 0-RTT for faster reconnection
			MaxIncomingStreams:               defaultMaxIncomingStreams,
			MaxIncomingUniStreams:            defaultMaxIncomingUniStreams,
		}
	}

	e := &endpointWebTransport{
		node: node,
		conf: conf,
	}
	initErr := e.initialize()
	return e, initErr
}

type endpointWebTransport struct {
	node *Node
	conf EndpointWebTransport

	ctx       context.Context
	cancel    context.CancelFunc
	terminate chan struct{}
	mu        sync.Mutex

	// Instance-level logger (never nil after initialization)
	logger WebTransportLogger

	// Connection state
	state   ConnectionState
	session *webtransport.Session
	dialer  *webtransport.Dialer

	// Reconnection state - uses common retry infrastructure
	retryState      *RetryState
	lastConnectTime time.Time
}

func (e *endpointWebTransport) initialize() error {
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.terminate = make(chan struct{})

	// Set logger - use config logger, fall back to default
	if e.conf.Logger != nil {
		e.logger = e.conf.Logger
	} else {
		e.logger = getDefaultWTLogger()
	}

	e.retryState = NewRetryState(RetryPolicy{
		InitialRetryPeriod:   e.conf.InitialRetryPeriod,
		MaxRetryPeriod:       e.conf.MaxRetryPeriod,
		BackoffMultiplier:    e.conf.BackoffMultiplier,
		MaxReconnectAttempts: e.conf.MaxReconnectAttempts,
	})
	e.setState(ConnStateDisconnected, nil)

	// Create WebTransport dialer
	e.dialer = &webtransport.Dialer{
		TLSClientConfig: e.conf.TLSConfig,
		QUICConfig:      e.conf.QUICConfig,
		DialAddr:        e.conf.DialAddr,
	}

	return nil
}

func (e *endpointWebTransport) close() {
	e.cancel()
	close(e.terminate)

	e.mu.Lock()
	if e.session != nil {
		e.session.CloseWithError(0, "endpoint closed")
	}
	e.mu.Unlock()
}

func (e *endpointWebTransport) isEndpoint() {}

func (e *endpointWebTransport) Conf() EndpointConf {
	return e.conf
}

func (e *endpointWebTransport) oneChannelAtAtime() bool {
	return true
}

func (e *endpointWebTransport) setState(newState ConnectionState, err error) {
	e.mu.Lock()
	oldState := e.state
	e.state = newState
	callback := e.conf.OnStateChange
	e.mu.Unlock()

	if callback != nil && oldState != newState {
		callback(oldState, newState, err)
	}
}

func (e *endpointWebTransport) provide() (string, io.ReadWriteCloser, error) {
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
				e.conf.URL, attempts, stats["consecutive_errors"], stats["current_retry_period"].(time.Duration).Milliseconds(), err)

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
			e.conf.URL, attempts, e.conf.UseDatagrams)

		label := e.conf.Label
		if label == "" {
			label = "webtransport"
		}

		return label, &removeCloser{rwc}, nil
	}
}

func (e *endpointWebTransport) connect() (io.ReadWriteCloser, error) {
	// Use HeaderProvider for dynamic headers (e.g., refreshed auth tokens),
	// falling back to static Headers map.
	var sourceHeaders map[string]string
	if e.conf.HeaderProvider != nil {
		var err error
		sourceHeaders, err = e.conf.HeaderProvider()
		if err != nil {
			return nil, fmt.Errorf("header provider failed: %w", err)
		}
	} else {
		sourceHeaders = e.conf.Headers
	}

	header := http.Header{}
	for k, v := range sourceHeaders {
		header.Set(k, v)
	}

	// Dial WebTransport session
	_, session, err := e.dialer.Dial(e.ctx, e.conf.URL, header)
	if err != nil {
		return nil, fmt.Errorf("failed to dial WebTransport: %w", err)
	}

	e.mu.Lock()
	e.session = session
	e.mu.Unlock()

	if e.conf.UseDatagrams {
		// Use unreliable datagrams for MAVLink (lower latency)
		e.logger.Printf("event=datagram_conn_open url=%s use_datagrams=true", e.conf.URL)
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
		session.CloseWithError(0, "failed to open stream")
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	return &webTransportStreamConn{
		session: session,
		stream:  stream,
	}, nil
}

// GetState returns the current connection state
func (e *endpointWebTransport) GetState() ConnectionState {
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
func (e *endpointWebTransport) GetStats() WebTransportStats {
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
	c.logger.Printf("event=datagram_conn_close duration_ms=%d datagrams_read=%d datagrams_written=%d bytes_read=%d bytes_written=%d read_errors=%d write_errors=%d",
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
