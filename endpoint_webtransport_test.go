package gomavlib

import (
	"crypto/tls"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
)

// TestWebTransportEndpoint_Init tests endpoint initialization
func TestWebTransportEndpoint_Init(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	require.NotNil(t, conf)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Verify defaults were set
	require.Equal(t, 1*time.Second, e.conf.InitialRetryPeriod)
	require.Equal(t, 30*time.Second, e.conf.MaxRetryPeriod)
	require.Equal(t, 1.5, e.conf.BackoffMultiplier)
	require.NotNil(t, e.conf.QUICConfig)
}

// TestWebTransportEndpoint_InitNoURL tests that missing URL returns error
func TestWebTransportEndpoint_InitNoURL(t *testing.T) {
	endpoint := EndpointWebTransport{}

	_, err := endpoint.init(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "URL is required")
}

// TestWebTransportEndpoint_InitWithCustomConfig tests custom configuration
func TestWebTransportEndpoint_InitWithCustomConfig(t *testing.T) {
	customQUICConfig := &quic.Config{
		MaxIdleTimeout:  60 * time.Second,
		KeepAlivePeriod: 15 * time.Second,
	}

	endpoint := EndpointWebTransport{
		URL:                "https://example.com/mavlink",
		InitialRetryPeriod: 2 * time.Second,
		MaxRetryPeriod:     60 * time.Second,
		BackoffMultiplier:  2.0,
		QUICConfig:         customQUICConfig,
		UseDatagrams:       true,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Verify custom values were kept
	require.Equal(t, 2*time.Second, e.conf.InitialRetryPeriod)
	require.Equal(t, 60*time.Second, e.conf.MaxRetryPeriod)
	require.Equal(t, 2.0, e.conf.BackoffMultiplier)
	require.Equal(t, customQUICConfig, e.conf.QUICConfig)
	require.True(t, e.conf.UseDatagrams)
}

// TestWebTransportEndpoint_Label tests the endpoint label
func TestWebTransportEndpoint_Label(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL:   "https://example.com/mavlink",
		Label: "custom-label",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	require.Equal(t, "custom-label", e.conf.Label)
}

// TestWebTransportEndpoint_Conf tests the Conf() method
func TestWebTransportEndpoint_Conf(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL:   "https://example.com/mavlink",
		Label: "test-endpoint",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	returnedConf := e.Conf().(EndpointWebTransport)
	require.Equal(t, endpoint.URL, returnedConf.URL)
	require.Equal(t, endpoint.Label, returnedConf.Label)
}

// TestWebTransportEndpoint_OneChannelAtATime tests oneChannelAtAtime() method
func TestWebTransportEndpoint_OneChannelAtATime(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// WebTransport endpoints should return true (one channel at a time)
	require.True(t, e.oneChannelAtAtime())
}

// TestWebTransportEndpoint_IsEndpoint tests isEndpoint() method
func TestWebTransportEndpoint_IsEndpoint(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// This is a marker method that should do nothing
	e.isEndpoint()
}

// TestWebTransportEndpoint_GetState tests GetState() method
func TestWebTransportEndpoint_GetState(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Initially should be disconnected
	state := e.GetState()
	require.Equal(t, ConnStateDisconnected, state)
}

// TestWebTransportEndpoint_GetStats tests GetStats() method
func TestWebTransportEndpoint_GetStats(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	stats := e.GetStats()
	require.Equal(t, "disconnected", stats.State)
	require.Equal(t, int32(0), stats.ReconnectAttempts)
	require.Equal(t, int32(0), stats.ConsecutiveErrors)
	require.True(t, stats.LastConnectTime.IsZero())
}

// TestWebTransportEndpoint_StateCallback tests OnStateChange callback
func TestWebTransportEndpoint_StateCallback(t *testing.T) {
	stateChanges := []ConnectionState{}
	var mu sync.Mutex
	connectingReceived := make(chan struct{})

	endpoint := EndpointWebTransport{
		URL:                  "https://127.0.0.1:9999/mavlink", // Non-existent server
		InitialRetryPeriod:   50 * time.Millisecond,
		MaxRetryPeriod:       100 * time.Millisecond,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 2,
		OnStateChange: func(oldState, newState ConnectionState, err error) {
			mu.Lock()
			stateChanges = append(stateChanges, newState)
			if newState == ConnStateConnecting {
				select {
				case <-connectingReceived:
				default:
					close(connectingReceived)
				}
			}
			mu.Unlock()
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)

	// Start connection in goroutine
	provideDone := make(chan struct{})
	go func() {
		defer close(provideDone)
		e.provide()
	}()

	// Wait for connecting state
	select {
	case <-connectingReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connecting state")
	}

	e.close()
	<-provideDone

	// Verify state changes were recorded
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, stateChanges, "should have recorded state changes")
	require.Contains(t, stateChanges, ConnStateConnecting)
}

// TestWebTransportEndpoint_TerminateReturnsErrTerminated tests that close causes errTerminated
func TestWebTransportEndpoint_TerminateReturnsErrTerminated(t *testing.T) {
	connecting := make(chan struct{})

	endpoint := EndpointWebTransport{
		URL:                  "https://127.0.0.1:9999/mavlink", // Non-existent server
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       200 * time.Millisecond,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0, // Unlimited
		OnStateChange: func(old, new ConnectionState, err error) {
			if new == ConnStateConnecting {
				select {
				case <-connecting:
				default:
					close(connecting)
				}
			}
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)

	// Start connection in goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for connecting state then close
	select {
	case <-connecting:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connecting state")
	}
	e.close()

	// Wait for provide to finish
	select {
	case <-provideDone:
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebTransportEndpoint_MaxReconnectAttempts tests max reconnect limit
func TestWebTransportEndpoint_MaxReconnectAttempts(t *testing.T) {
	disconnected := make(chan struct{})

	endpoint := EndpointWebTransport{
		URL:                  "https://127.0.0.1:9999/mavlink", // Non-existent server
		InitialRetryPeriod:   50 * time.Millisecond,
		MaxRetryPeriod:       100 * time.Millisecond,
		BackoffMultiplier:    1.0, // No backoff for faster test
		MaxReconnectAttempts: 3,
		OnStateChange: func(old, new ConnectionState, err error) {
			if new == ConnStateDisconnected && old != ConnStateDisconnected {
				select {
				case <-disconnected:
				default:
					close(disconnected)
				}
			}
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)

	// Start connection in goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for disconnected state after max attempts
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnected state")
	}

	// Verify state is disconnected after max attempts
	state := e.GetState()
	require.Equal(t, ConnStateDisconnected, state)

	e.close()

	// Wait for provide to finish
	select {
	case <-provideDone:
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebTransportEndpoint_BackoffIncrease tests exponential backoff calculation
func TestWebTransportEndpoint_BackoffIncrease(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL:                  "https://example.com/mavlink",
		InitialRetryPeriod:   50 * time.Millisecond,
		MaxRetryPeriod:       200 * time.Millisecond,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 5,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Initial period should be 50ms
	require.Equal(t, 50*time.Millisecond, e.currentRetryPeriod)

	// Test backoff calculation by simulating the formula used in provide()
	// First backoff: 50 * 2.0 = 100ms
	nextPeriod := time.Duration(float64(e.currentRetryPeriod) * e.conf.BackoffMultiplier)
	nextPeriod = min(nextPeriod, e.conf.MaxRetryPeriod)
	require.Equal(t, 100*time.Millisecond, nextPeriod)

	// Second backoff: 100 * 2.0 = 200ms (at cap)
	nextPeriod = time.Duration(float64(nextPeriod) * e.conf.BackoffMultiplier)
	nextPeriod = min(nextPeriod, e.conf.MaxRetryPeriod)
	require.Equal(t, 200*time.Millisecond, nextPeriod)

	// Third backoff: 200 * 2.0 = 400ms but capped at 200ms
	nextPeriod = time.Duration(float64(nextPeriod) * e.conf.BackoffMultiplier)
	nextPeriod = min(nextPeriod, e.conf.MaxRetryPeriod)
	require.Equal(t, 200*time.Millisecond, nextPeriod) // Capped
}

// TestWebTransportEndpoint_TLSConfig tests custom TLS configuration
func TestWebTransportEndpoint_TLSConfig(t *testing.T) {
	customTLS := &tls.Config{
		InsecureSkipVerify: true,
	}

	endpoint := EndpointWebTransport{
		URL:       "https://example.com/mavlink",
		TLSConfig: customTLS,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	require.Equal(t, customTLS, e.conf.TLSConfig)
	require.True(t, e.dialer.TLSClientConfig.InsecureSkipVerify)
}

// TestWebTransportEndpoint_Headers tests custom headers
func TestWebTransportEndpoint_Headers(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
			"X-Custom":      "value",
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	require.Equal(t, "Bearer test-token", e.conf.Headers["Authorization"])
	require.Equal(t, "value", e.conf.Headers["X-Custom"])
}

// TestWebTransportEndpoint_UseDatagrams tests datagram mode configuration
func TestWebTransportEndpoint_UseDatagrams(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL:          "https://example.com/mavlink",
		UseDatagrams: true,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	require.True(t, e.conf.UseDatagrams)
	require.True(t, e.conf.QUICConfig.EnableDatagrams)
}

// TestWebTransportEndpoint_SetState tests state transitions
func TestWebTransportEndpoint_SetState(t *testing.T) {
	var lastOldState, lastNewState ConnectionState
	var lastErr error
	var mu sync.Mutex

	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
		OnStateChange: func(old, new ConnectionState, err error) {
			mu.Lock()
			lastOldState = old
			lastNewState = new
			lastErr = err
			mu.Unlock()
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Set state to connecting
	e.setState(ConnStateConnecting, nil)

	mu.Lock()
	require.Equal(t, ConnStateDisconnected, lastOldState)
	require.Equal(t, ConnStateConnecting, lastNewState)
	require.Nil(t, lastErr)
	mu.Unlock()

	// Set state to connected
	e.setState(ConnStateConnected, nil)

	mu.Lock()
	require.Equal(t, ConnStateConnecting, lastOldState)
	require.Equal(t, ConnStateConnected, lastNewState)
	mu.Unlock()

	// Set state to disconnected with error
	testErr := errors.New("connection lost")
	e.setState(ConnStateDisconnected, testErr)

	mu.Lock()
	require.Equal(t, ConnStateConnected, lastOldState)
	require.Equal(t, ConnStateDisconnected, lastNewState)
	require.Equal(t, testErr, lastErr)
	mu.Unlock()
}

// TestWebTransportEndpoint_SetStateSameState tests that callback is not called for same state
func TestWebTransportEndpoint_SetStateSameState(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
		OnStateChange: func(old, new ConnectionState, err error) {
			mu.Lock()
			callCount++
			mu.Unlock()
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Set to same state (disconnected -> disconnected)
	e.setState(ConnStateDisconnected, nil)

	mu.Lock()
	require.Equal(t, 0, callCount, "callback should not be called for same state")
	mu.Unlock()
}

// TestWebTransportEndpoint_DefaultQUICConfig tests default QUIC configuration
func TestWebTransportEndpoint_DefaultQUICConfig(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL:          "https://example.com/mavlink",
		UseDatagrams: true,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Verify default QUIC config values using constants
	require.Equal(t, defaultMaxIdleTimeout, e.conf.QUICConfig.MaxIdleTimeout)
	require.Equal(t, defaultKeepAlivePeriod, e.conf.QUICConfig.KeepAlivePeriod)
	require.True(t, e.conf.QUICConfig.EnableDatagrams)
	require.True(t, e.conf.QUICConfig.Allow0RTT)
	require.Equal(t, int64(defaultMaxIncomingStreams), e.conf.QUICConfig.MaxIncomingStreams)
	require.Equal(t, int64(defaultMaxIncomingUniStreams), e.conf.QUICConfig.MaxIncomingUniStreams)
}

// TestWebTransportEndpoint_DefaultLabel tests default label
func TestWebTransportEndpoint_DefaultLabel(t *testing.T) {
	endpoint := EndpointWebTransport{
		URL: "https://example.com/mavlink",
		// No Label set
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)

	e := conf.(*endpointWebTransport)
	defer e.close()

	// Default label should be empty in config, but "webtransport" is returned by provide()
	require.Equal(t, "", e.conf.Label)
}

// TestWebTransportDatagramConn_ClosedRead tests reading from closed datagram connection
func TestWebTransportDatagramConn_ClosedRead(t *testing.T) {
	conn := &webTransportDatagramConn{
		closed: true,
	}

	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// TestWebTransportDatagramConn_ClosedWrite tests writing to closed datagram connection
func TestWebTransportDatagramConn_ClosedWrite(t *testing.T) {
	conn := &webTransportDatagramConn{
		closed: true,
	}

	n, err := conn.Write([]byte("test"))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// TestWebTransportDatagramConn_DoubleClose tests that double close returns nil
func TestWebTransportDatagramConn_DoubleClose(t *testing.T) {
	conn := &webTransportDatagramConn{
		closed: true,
	}

	err := conn.Close()
	require.NoError(t, err)
}

// TestWebTransportStreamConn_ClosedRead tests reading from closed stream connection
func TestWebTransportStreamConn_ClosedRead(t *testing.T) {
	conn := &webTransportStreamConn{
		closed: true,
	}

	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// TestWebTransportStreamConn_ClosedWrite tests writing to closed stream connection
func TestWebTransportStreamConn_ClosedWrite(t *testing.T) {
	conn := &webTransportStreamConn{
		closed: true,
	}

	n, err := conn.Write([]byte("test"))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// TestWebTransportStreamConn_DoubleClose tests that double close returns nil
func TestWebTransportStreamConn_DoubleClose(t *testing.T) {
	conn := &webTransportStreamConn{
		closed: true,
	}

	err := conn.Close()
	require.NoError(t, err)
}

// TestWebTransportEndpoint_URLSchemeValidation tests URL scheme validation
func TestWebTransportEndpoint_URLSchemeValidation(t *testing.T) {
	testCases := []struct {
		name        string
		url         string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid https URL",
			url:         "https://example.com/mavlink",
			expectError: false,
		},
		{
			name:        "invalid http URL",
			url:         "http://example.com/mavlink",
			expectError: true,
			errorMsg:    "must use https://",
		},
		{
			name:        "invalid ws URL",
			url:         "ws://example.com/mavlink",
			expectError: true,
			errorMsg:    "must use https://",
		},
		{
			name:        "invalid wss URL",
			url:         "wss://example.com/mavlink",
			expectError: true,
			errorMsg:    "must use https://",
		},
		{
			name:        "empty URL",
			url:         "",
			expectError: true,
			errorMsg:    "URL is required",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := EndpointWebTransport{
				URL: tc.url,
			}

			_, err := endpoint.init(nil)
			if tc.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
