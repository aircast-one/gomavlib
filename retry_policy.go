package gomavlib

import (
	"sync"
	"sync/atomic"
	"time"
)

// RetryPolicy defines the configuration for automatic reconnection with exponential backoff
type RetryPolicy struct {
	// InitialRetryPeriod is the initial delay before first retry (default: 1s)
	InitialRetryPeriod time.Duration

	// MaxRetryPeriod is the maximum delay between retries (default: 30s)
	MaxRetryPeriod time.Duration

	// BackoffMultiplier is the factor to multiply the delay after each attempt (default: 1.5)
	BackoffMultiplier float64

	// MaxReconnectAttempts is the maximum number of reconnection attempts (0 = unlimited)
	MaxReconnectAttempts int

	// Circuit breaker configuration (optional, set to 0 to disable)

	// MaxConsecutiveErrors triggers circuit breaker when reached (default: 0 = disabled)
	MaxConsecutiveErrors int

	// CircuitBreakerTimeout is how long the circuit breaker stays open (default: 5 minutes)
	CircuitBreakerTimeout time.Duration
}

// DefaultRetryPolicy returns a retry policy with sensible defaults
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		InitialRetryPeriod:   1 * time.Second,
		MaxRetryPeriod:       30 * time.Second,
		BackoffMultiplier:    1.5,
		MaxReconnectAttempts: 0, // unlimited
	}
}

// ApplyDefaults fills in any zero values with defaults
func (p *RetryPolicy) ApplyDefaults() {
	defaults := DefaultRetryPolicy()
	if p.InitialRetryPeriod == 0 {
		p.InitialRetryPeriod = defaults.InitialRetryPeriod
	}
	if p.MaxRetryPeriod == 0 {
		p.MaxRetryPeriod = defaults.MaxRetryPeriod
	}
	if p.BackoffMultiplier == 0 {
		p.BackoffMultiplier = defaults.BackoffMultiplier
	}
}

// RetryState tracks the current state of a retry loop.
// Thread-safe: uses atomic operations for counters and mutex for non-atomic fields.
type RetryState struct {
	policy RetryPolicy

	// Mutex protects non-atomic fields (currentRetryPeriod, circuitBreakerOpenAt)
	mu                 sync.Mutex
	currentRetryPeriod time.Duration
	circuitBreakerOpenAt time.Time

	// Atomic counters for high-frequency operations
	reconnectAttempts int32
	consecutiveErrors int32
}

// NewRetryState creates a new retry state with the given policy
func NewRetryState(policy RetryPolicy) *RetryState {
	policy.ApplyDefaults()
	return &RetryState{
		policy:             policy,
		currentRetryPeriod: policy.InitialRetryPeriod,
	}
}

// BeforeAttempt should be called before each connection attempt.
// Returns true if we should wait before connecting (i.e., this is a retry).
// The waitDuration is how long to wait before attempting.
// After waiting, the backoff period is increased for the next potential retry.
// Thread-safe.
func (s *RetryState) BeforeAttempt() (shouldWait bool, waitDuration time.Duration) {
	attempts := atomic.LoadInt32(&s.reconnectAttempts)
	if attempts > 0 {
		s.mu.Lock()
		// Return current wait period
		wait := s.currentRetryPeriod

		// Increase backoff for next time (this matches original behavior:
		// wait with current period, then increase)
		nextPeriod := time.Duration(float64(s.currentRetryPeriod) * s.policy.BackoffMultiplier)
		s.currentRetryPeriod = min(nextPeriod, s.policy.MaxRetryPeriod)
		s.mu.Unlock()

		return true, wait
	}
	return false, 0
}

// RecordAttempt should be called when starting a connection attempt
func (s *RetryState) RecordAttempt() {
	atomic.AddInt32(&s.reconnectAttempts, 1)
}

// RecordSuccess should be called when a connection succeeds.
// Thread-safe.
func (s *RetryState) RecordSuccess() {
	atomic.StoreInt32(&s.consecutiveErrors, 0)
	s.mu.Lock()
	s.currentRetryPeriod = s.policy.InitialRetryPeriod
	s.circuitBreakerOpenAt = time.Time{} // Close circuit breaker
	s.mu.Unlock()
}

// RecordError should be called when a connection fails.
// Returns true if we should continue retrying.
func (s *RetryState) RecordError() bool {
	consecutiveErrors := atomic.AddInt32(&s.consecutiveErrors, 1)

	// Check circuit breaker (if configured)
	if s.policy.MaxConsecutiveErrors > 0 && int(consecutiveErrors) >= s.policy.MaxConsecutiveErrors {
		s.openCircuitBreaker()
		return false
	}

	// Check max attempts
	if s.policy.MaxReconnectAttempts > 0 {
		attempts := atomic.LoadInt32(&s.reconnectAttempts)
		if int(attempts) >= s.policy.MaxReconnectAttempts {
			return false
		}
	}

	return true
}

// IsCircuitBreakerOpen returns true if the circuit breaker is currently open.
// Thread-safe.
func (s *RetryState) IsCircuitBreakerOpen() bool {
	s.mu.Lock()
	openAt := s.circuitBreakerOpenAt
	s.mu.Unlock()

	if openAt.IsZero() {
		return false
	}
	if s.policy.CircuitBreakerTimeout == 0 {
		return false // No timeout configured means circuit breaker is disabled
	}
	return time.Since(openAt) < s.policy.CircuitBreakerTimeout
}

// CircuitBreakerWaitTime returns remaining time until circuit breaker closes.
// Thread-safe.
func (s *RetryState) CircuitBreakerWaitTime() time.Duration {
	s.mu.Lock()
	openAt := s.circuitBreakerOpenAt
	s.mu.Unlock()

	if openAt.IsZero() || s.policy.CircuitBreakerTimeout == 0 {
		return 0
	}
	remaining := s.policy.CircuitBreakerTimeout - time.Since(openAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// openCircuitBreaker opens the circuit breaker (internal, caller must hold mutex or use exported method)
func (s *RetryState) openCircuitBreaker() {
	s.mu.Lock()
	s.circuitBreakerOpenAt = time.Now()
	s.mu.Unlock()
}

// CloseCircuitBreaker manually closes the circuit breaker (e.g., on successful connection).
// Thread-safe.
func (s *RetryState) CloseCircuitBreaker() {
	s.mu.Lock()
	s.circuitBreakerOpenAt = time.Time{}
	s.mu.Unlock()
}

// OpenCircuitBreaker manually opens the circuit breaker (for testing).
// Thread-safe.
func (s *RetryState) OpenCircuitBreaker() {
	s.openCircuitBreaker()
}

// GetStats returns current retry statistics.
// Thread-safe.
func (s *RetryState) GetStats() map[string]any {
	s.mu.Lock()
	currentPeriod := s.currentRetryPeriod
	s.mu.Unlock()

	return map[string]any{
		"reconnect_attempts":     atomic.LoadInt32(&s.reconnectAttempts),
		"consecutive_errors":     atomic.LoadInt32(&s.consecutiveErrors),
		"current_retry_period":   currentPeriod,
		"max_reconnect_attempts": s.policy.MaxReconnectAttempts,
		"circuit_breaker_open":   s.IsCircuitBreakerOpen(),
	}
}

// ReconnectAttempts returns the current reconnect attempt count
func (s *RetryState) ReconnectAttempts() int32 {
	return atomic.LoadInt32(&s.reconnectAttempts)
}

// ConsecutiveErrors returns the current consecutive error count
func (s *RetryState) ConsecutiveErrors() int32 {
	return atomic.LoadInt32(&s.consecutiveErrors)
}
