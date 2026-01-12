package gomavlib

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

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

	// Optional HTTP headers for the WebTransport handshake
	Headers map[string]string

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

	// State change callback (optional)
	OnStateChange ConnectionStateCallback
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
			MaxIdleTimeout:        defaultMaxIdleTimeout,
			KeepAlivePeriod:       defaultKeepAlivePeriod,
			EnableDatagrams:       conf.UseDatagrams,
			Allow0RTT:             true, // Enable 0-RTT for faster reconnection
			MaxIncomingStreams:    defaultMaxIncomingStreams,
			MaxIncomingUniStreams: defaultMaxIncomingUniStreams,
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

	// Connection state
	state   ConnectionState
	session *webtransport.Session
	dialer  *webtransport.Dialer

	// Reconnection state
	reconnectAttempts  int32
	consecutiveErrors  int32
	currentRetryPeriod time.Duration
	lastConnectTime    time.Time
}

func (e *endpointWebTransport) initialize() error {
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.terminate = make(chan struct{})
	e.currentRetryPeriod = e.conf.InitialRetryPeriod
	e.setState(ConnStateDisconnected, nil)

	// Create WebTransport dialer
	e.dialer = &webtransport.Dialer{
		TLSClientConfig: e.conf.TLSConfig,
		QUICConfig:      e.conf.QUICConfig,
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

		attempts := atomic.LoadInt32(&e.reconnectAttempts)
		if attempts > 0 {
			e.setState(ConnStateReconnecting, nil)

			select {
			case <-time.After(e.currentRetryPeriod):
			case <-e.terminate:
				return "", nil, errTerminated
			}

			nextPeriod := time.Duration(float64(e.currentRetryPeriod) * e.conf.BackoffMultiplier)
			e.currentRetryPeriod = min(nextPeriod, e.conf.MaxRetryPeriod)
		} else {
			e.setState(ConnStateConnecting, nil)
		}

		atomic.AddInt32(&e.reconnectAttempts, 1)

		// Attempt connection
		rwc, err := e.connect()
		if err != nil {
			atomic.AddInt32(&e.consecutiveErrors, 1)

			// Check max reconnect attempts
			if e.conf.MaxReconnectAttempts > 0 &&
				int(atomic.LoadInt32(&e.reconnectAttempts)) >= e.conf.MaxReconnectAttempts {
				e.setState(ConnStateDisconnected, err)
				<-e.terminate
				return "", nil, errTerminated
			}

			continue
		}

		// Connection successful
		atomic.StoreInt32(&e.consecutiveErrors, 0)
		e.mu.Lock()
		e.currentRetryPeriod = e.conf.InitialRetryPeriod
		e.lastConnectTime = time.Now()
		e.mu.Unlock()
		e.setState(ConnStateConnected, nil)

		label := e.conf.Label
		if label == "" {
			label = "webtransport"
		}

		return label, &removeCloser{rwc}, nil
	}
}

func (e *endpointWebTransport) connect() (io.ReadWriteCloser, error) {
	// Build request headers
	header := http.Header{}
	for k, v := range e.conf.Headers {
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
		return &webTransportDatagramConn{
			session: session,
			ctx:     e.ctx,
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
		ReconnectAttempts: atomic.LoadInt32(&e.reconnectAttempts),
		ConsecutiveErrors: atomic.LoadInt32(&e.consecutiveErrors),
		LastConnectTime:   lastConnect,
	}
}

// webTransportDatagramConn adapts WebTransport datagrams to io.ReadWriteCloser
type webTransportDatagramConn struct {
	session *webtransport.Session
	ctx     context.Context
	mu      sync.Mutex
	closed  bool
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
		return 0, err
	}

	if len(data) > len(p) {
		// Copy what fits and return error to indicate truncation
		copy(p, data[:len(p)])
		return len(p), ErrDatagramTruncated
	}

	return copy(p, data), nil
}

func (c *webTransportDatagramConn) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	err = c.session.SendDatagram(p)
	if err != nil {
		return 0, err
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
