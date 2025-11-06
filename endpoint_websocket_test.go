package gomavlib

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestWebSocketEndpoint_AuthError tests that auth errors cause the endpoint
// to block and return errTerminated instead of panicking
func TestWebSocketEndpoint_AuthError(t *testing.T) {
	// Create a server that returns 401 Unauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Unauthorized"))
	}))
	defer server.Close()

	// Replace http:// with ws://
	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0, // Unlimited attempts
		HandshakeTimeout:     1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait a bit for the auth error to occur
	time.Sleep(300 * time.Millisecond)

	// Verify endpoint is in disconnected or reconnecting state (auth errors are treated as network errors)
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	// Auth errors from HTTP status codes are categorized as network errors, so reconnecting
	require.Contains(t, []WebSocketState{WSStateDisconnected, WSStateReconnecting, WSStateConnecting}, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// Should return errTerminated, not panic
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_CircuitBreakerOpen tests that when circuit breaker opens,
// the endpoint blocks and returns errTerminated instead of panicking
func TestWebSocketEndpoint_CircuitBreakerOpen(t *testing.T) {
	// Use invalid port so connections fail immediately
	wsURL := "ws://127.0.0.1:9999/test"

	endpoint := EndpointWebSocket{
		URL:                   wsURL,
		InitialRetryPeriod:    50 * time.Millisecond,
		MaxRetryPeriod:        100 * time.Millisecond,
		BackoffMultiplier:     2.0,
		MaxReconnectAttempts:  0, // Unlimited attempts
		HandshakeTimeout:      100 * time.Millisecond,
		MaxConsecutiveErrors:  3, // Open circuit breaker after 3 errors
		CircuitBreakerTimeout: 1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for circuit breaker to open (3 consecutive errors)
	time.Sleep(500 * time.Millisecond)

	// Verify circuit breaker is open
	e.mu.Lock()
	isOpen := e.isCircuitBreakerOpen()
	e.mu.Unlock()
	require.True(t, isOpen, "circuit breaker should be open after consecutive errors")

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// Should return errTerminated, not panic
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_SuccessfulConnection tests normal connection flow
func TestWebSocketEndpoint_SuccessfulConnection(t *testing.T) {
	// Create a WebSocket server
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Keep connection open for a bit
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideLabel string
	var provideErr error
	go func() {
		defer close(provideDone)
		provideLabel, _, provideErr = e.provide()
	}()

	// Wait for connection to establish
	time.Sleep(200 * time.Millisecond)

	// Verify endpoint is in connected state
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	require.Equal(t, WSStateConnected, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// provide() may return nil if connection was established before close,
		// or errTerminated if closed during connection
		if provideErr != nil {
			require.ErrorIs(t, provideErr, errTerminated)
		}
		if provideErr == nil {
			require.NotEmpty(t, provideLabel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_ReconnectAfterDisconnect tests reconnection logic
func TestWebSocketEndpoint_ReconnectAfterDisconnect(t *testing.T) {
	connectionCount := 0
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectionCount++

		// First connection: close immediately (simulate network issue)
		if connectionCount == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// Second connection: succeed
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Keep connection open
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       500 * time.Millisecond,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for reconnection to succeed
	time.Sleep(500 * time.Millisecond)

	// Verify we got at least 2 connection attempts
	require.GreaterOrEqual(t, connectionCount, 2, "should have attempted reconnection")

	// Verify endpoint is in connected state
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	require.Equal(t, WSStateConnected, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// provide() may return nil if connection was established before close,
		// or errTerminated if closed during connection/reconnection
		if provideErr != nil {
			require.ErrorIs(t, provideErr, errTerminated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_OnlyReturnsErrTerminated verifies that provide() never
// returns errors other than errTerminated (gomavlib contract)
func TestWebSocketEndpoint_OnlyReturnsErrTerminated(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode int
		setupFunc  func(e *endpointWebSocket)
	}{
		{
			name:       "401 Unauthorized",
			statusCode: http.StatusUnauthorized,
		},
		{
			name:       "403 Forbidden",
			statusCode: http.StatusForbidden,
		},
		{
			name:       "404 Not Found",
			statusCode: http.StatusNotFound,
		},
		{
			name: "Circuit Breaker Open",
			setupFunc: func(e *endpointWebSocket) {
				// Force circuit breaker to open
				e.mu.Lock()
				e.circuitBreakerOpenAt = time.Now()
				e.mu.Unlock()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create server that returns the specified status code
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.statusCode != 0 {
					w.WriteHeader(tc.statusCode)
				}
			}))
			defer server.Close()

			wsURL := "ws" + server.URL[4:]

			endpoint := EndpointWebSocket{
				URL:                   wsURL,
				InitialRetryPeriod:    50 * time.Millisecond,
				MaxRetryPeriod:        100 * time.Millisecond,
				BackoffMultiplier:     2.0,
				MaxReconnectAttempts:  0,
				HandshakeTimeout:      200 * time.Millisecond,
				MaxConsecutiveErrors:  3,
				CircuitBreakerTimeout: 1 * time.Second,
			}

			conf, err := endpoint.init(nil)
			require.NoError(t, err)
			e := conf.(*endpointWebSocket)

			if tc.setupFunc != nil {
				tc.setupFunc(e)
			}

			// Start the endpoint in a goroutine
			provideDone := make(chan struct{})
			var provideErr error
			go func() {
				defer close(provideDone)
				_, _, provideErr = e.provide()
			}()

			// Wait a bit for the error condition to occur
			time.Sleep(200 * time.Millisecond)

			// Close the endpoint
			e.close()

			// Wait for provide() to return
			select {
			case <-provideDone:
				// CRITICAL: Must only return errTerminated, never other errors
				if provideErr != nil && !errors.Is(provideErr, errTerminated) {
					t.Fatalf("provide() returned unexpected error: %v (expected errTerminated or nil)", provideErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provide() did not return after close")
			}
		})
	}
}

// TestWebSocketEndpoint_Label tests the endpoint label
func TestWebSocketEndpoint_Label(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL:   "ws://localhost:8080/test",
		Label: "test-label",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	// The label is stored in conf
	require.Equal(t, "test-label", e.conf.Label)
}

// TestWebSocketEndpoint_Conf tests the Conf() method
func TestWebSocketEndpoint_Conf(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL:   "ws://localhost:8080/test",
		Label: "test-endpoint",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	returnedConf := e.Conf().(EndpointWebSocket)
	// Just verify it returns a config (defaults may be filled in)
	require.NotEqual(t, EndpointWebSocket{}, returnedConf)
	require.Equal(t, endpoint.URL, returnedConf.URL)
	require.Equal(t, endpoint.Label, returnedConf.Label)
}

// TestWebSocketEndpoint_OneChannelAtATime tests oneChannelAtAtime() method
func TestWebSocketEndpoint_OneChannelAtATime(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL: "ws://localhost:8080/test",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	// WebSocket endpoints should return true (one channel at a time)
	require.True(t, e.oneChannelAtAtime())
}

// TestWebSocketEndpoint_IsEndpoint tests isEndpoint() method
func TestWebSocketEndpoint_IsEndpoint(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL: "ws://localhost:8080/test",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	// This is a marker method that should do nothing
	e.isEndpoint()
}

// TestWebSocketEndpoint_GetState tests GetState() method
func TestWebSocketEndpoint_GetState(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL:                  "ws://localhost:8080/test",
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	// Initially should be disconnected
	state := e.GetState()
	require.Equal(t, WSStateDisconnected, state)
}

// TestWebSocketEndpoint_IsHealthy tests IsHealthy() method
func TestWebSocketEndpoint_IsHealthy(t *testing.T) {
	// Create a WebSocket server
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(1 * time.Second)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start connection
	go e.provide()

	// Wait for connection
	time.Sleep(300 * time.Millisecond)

	// Should be healthy when connected
	healthy := e.IsHealthy()
	require.True(t, healthy)

	e.close()
}

// TestWebSocketEndpoint_GetStats tests GetStats() method
func TestWebSocketEndpoint_GetStats(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL:                  "ws://localhost:8080/test",
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	stats := e.GetStats()
	require.NotNil(t, stats)
	// Stats returns a map with state as string
	require.Equal(t, "disconnected", stats["state"])
	require.Equal(t, int32(0), stats["reconnect_attempts"])
}

// TestWebSocketEndpoint_CategorizeError tests error categorization
func TestWebSocketEndpoint_CategorizeError(t *testing.T) {
	endpoint := EndpointWebSocket{
		URL: "ws://localhost:8080/test",
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)
	defer e.close()

	// Test that categorizeError doesn't crash on various error types
	_ = e.categorizeError(errors.New("i/o timeout"))
	_ = e.categorizeError(errors.New("connection refused"))
	_ = e.categorizeError(errors.New("dial tcp: error"))
	_ = e.categorizeError(errors.New("some other error"))
}

// TestWebSocketEndpoint_PingPongMechanism tests ping/pong keepalive
func TestWebSocketEndpoint_PingPongMechanism(t *testing.T) {
	pingReceived := false

	// Create a WebSocket server that responds to pings
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.SetPingHandler(func(appData string) error {
			pingReceived = true
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})

		// Keep connection alive
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           200 * time.Millisecond, // Short ping period for testing
		PongWait:             1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start connection
	go e.provide()

	// Wait for ping to be sent
	time.Sleep(500 * time.Millisecond)

	e.close()

	// Verify ping was received (pinger is working)
	require.True(t, pingReceived, "server should have received ping")
}

// TestWebSocketEndpoint_StateCallback tests OnStateChange callback
func TestWebSocketEndpoint_StateCallback(t *testing.T) {
	stateChanges := []WebSocketState{}
	var mu sync.Mutex

	// Create a WebSocket server
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(300 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
		OnStateChange: func(oldState, newState WebSocketState, err error) {
			mu.Lock()
			stateChanges = append(stateChanges, newState)
			mu.Unlock()
		},
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start connection
	go e.provide()

	// Wait for connection to establish
	time.Sleep(300 * time.Millisecond)

	e.close()

	// Verify state changes were recorded
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, stateChanges, "should have recorded state changes")
	require.Contains(t, stateChanges, WSStateConnecting)
	require.Contains(t, stateChanges, WSStateConnected)
}

// TestWebSocketEndpoint_ReadWriteOperations tests Read/Write on webSocketConn
func TestWebSocketEndpoint_ReadWriteOperations(t *testing.T) {
	receivedData := make(chan []byte, 1)

	// Create a WebSocket server that echoes messages
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read one message
		_, msg, err := conn.ReadMessage()
		if err == nil {
			receivedData <- msg
			// Echo it back
			conn.WriteMessage(websocket.BinaryMessage, msg)
		}

		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start connection
	done := make(chan struct{})
	var rwc io.ReadWriteCloser
	go func() {
		defer close(done)
		_, rwc, _ = e.provide()
	}()

	// Wait for connection
	time.Sleep(200 * time.Millisecond)

	// Write data
	testData := []byte("test message")
	if rwc != nil {
		n, err := rwc.Write(testData)
		require.NoError(t, err)
		require.Equal(t, len(testData), n)
	}

	// Verify server received it
	select {
	case data := <-receivedData:
		require.Equal(t, testData, data)
	case <-time.After(1 * time.Second):
		t.Fatal("server did not receive data")
	}

	e.close()
	<-done
}

// TestWebSocketEndpoint_HeadersSupport tests custom headers
func TestWebSocketEndpoint_HeadersSupport(t *testing.T) {
	receivedAuth := ""

	// Create a WebSocket server that checks auth header
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL: wsURL,
		Headers: map[string]string{
			"Authorization": "Bearer test-token-123",
		},
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start connection
	go e.provide()

	// Wait for connection
	time.Sleep(300 * time.Millisecond)

	e.close()

	// Verify custom header was sent
	require.Equal(t, "Bearer test-token-123", receivedAuth)
}
