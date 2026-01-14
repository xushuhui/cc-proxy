package main

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"
)

// BackendState tracks runtime state of a backend
type BackendState struct {
	backend          Backend
	consecutiveFails int
	lastFailTime     time.Time
	lastError        string
	circuitOpen      bool
	last429Time      time.Time
	retryAfter       time.Time
	halfOpenTries    int
}

// CircuitBreaker manages circuit breaker logic for all backends
type CircuitBreaker struct {
	config  *Config
	states  []*BackendState
	stateMu sync.RWMutex
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(config *Config) *CircuitBreaker {
	states := make([]*BackendState, len(config.Backends))
	for i, backend := range config.Backends {
		states[i] = &BackendState{
			backend: backend,
		}
	}

	return &CircuitBreaker{
		config: config,
		states: states,
	}
}

// ShouldSkipBackend checks if backend should be skipped based on circuit breaker and rate limit
func (cb *CircuitBreaker) ShouldSkipBackend(state *BackendState) (bool, string) {
	cb.stateMu.RLock()
	defer cb.stateMu.RUnlock()

	now := time.Now()

	// Check circuit breaker
	if state.circuitOpen {
		openDuration := now.Sub(state.lastFailTime)
		timeout := time.Duration(cb.config.Failover.CircuitBreaker.OpenTimeoutSeconds) * time.Second

		if openDuration < timeout {
			return true, fmt.Sprintf("熔断中 (还需 %.0f 秒)", timeout.Seconds()-openDuration.Seconds())
		}

		// Circuit breaker timeout expired, check half-open state
		maxHalfOpenRequests := cb.config.Failover.CircuitBreaker.HalfOpenRequests
		if state.halfOpenTries >= maxHalfOpenRequests {
			// Already used up all half-open test requests, keep skipping
			return true, fmt.Sprintf("半开测试中 (已用 %d/%d 次)", state.halfOpenTries, maxHalfOpenRequests)
		}
		// Allow this half-open test request
	}

	// Check rate limit cooldown (429)
	if !state.last429Time.IsZero() {
		cooldown := time.Duration(cb.config.Failover.RateLimit.CooldownSeconds) * time.Second
		if now.Sub(state.last429Time) < cooldown {
			// Don't completely skip, but this backend has lower priority
			// We'll still try it if all others fail
			return false, ""
		}
	}

	return false, ""
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess(state *BackendState) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	if state.circuitOpen {
		log.Printf("[熔断恢复] %s - 后端已恢复正常", state.backend.Name)
	}

	state.consecutiveFails = 0
	state.circuitOpen = false
	state.halfOpenTries = 0
	state.lastFailTime = time.Time{}
}

// RecordFailure records a failed request (5xx errors or network errors)
func (cb *CircuitBreaker) RecordFailure(state *BackendState, statusCode int) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	state.consecutiveFails++
	state.lastFailTime = time.Now()
	state.lastError = fmt.Sprintf("HTTP %d", statusCode)

	if state.circuitOpen {
		// Already open, reset half-open counter
		state.halfOpenTries = 0
		log.Printf("[熔断测试失败] %s - 继续熔断 %d 秒", state.backend.Name, cb.config.Failover.CircuitBreaker.OpenTimeoutSeconds)
		return
	}

	// Check if threshold reached
	if state.consecutiveFails >= cb.config.Failover.CircuitBreaker.FailureThreshold {
		state.circuitOpen = true
		timeout := cb.config.Failover.CircuitBreaker.OpenTimeoutSeconds
		log.Printf("[熔断触发] %s - 连续失败 %d 次,熔断 %d 秒 (HTTP %d)",
			state.backend.Name, state.consecutiveFails, timeout, statusCode)
	}
}

// IncrementHalfOpenTries increments the half-open test counter
func (cb *CircuitBreaker) IncrementHalfOpenTries(state *BackendState) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	state.halfOpenTries++
}

// IsHalfOpen checks if backend is in half-open state
func (cb *CircuitBreaker) IsHalfOpen(state *BackendState) bool {
	cb.stateMu.RLock()
	defer cb.stateMu.RUnlock()

	if !state.circuitOpen {
		return false
	}

	timeout := time.Duration(cb.config.Failover.CircuitBreaker.OpenTimeoutSeconds) * time.Second
	return time.Since(state.lastFailTime) >= timeout
}

// Record429 records a rate limit error
func (cb *CircuitBreaker) Record429(state *BackendState, retryAfter string) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	state.last429Time = time.Now()

	// Parse Retry-After header if present
	if retryAfter != "" {
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			state.retryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
			log.Printf("[限流记录] %s - 触发 429,Retry-After: %d 秒", state.backend.Name, seconds)
		} else {
			log.Printf("[限流记录] %s - 触发 429,冷却 %d 秒", state.backend.Name, cb.config.Failover.RateLimit.CooldownSeconds)
		}
	} else {
		log.Printf("[限流记录] %s - 触发 429,冷却 %d 秒", state.backend.Name, cb.config.Failover.RateLimit.CooldownSeconds)
	}
}

// SortBackendsByPriority returns backends sorted by priority (non-rate-limited first)
func (cb *CircuitBreaker) SortBackendsByPriority() []*BackendState {
	cb.stateMu.RLock()
	defer cb.stateMu.RUnlock()

	now := time.Now()
	cooldown := time.Duration(cb.config.Failover.RateLimit.CooldownSeconds) * time.Second

	normal := make([]*BackendState, 0)
	rateLimited := make([]*BackendState, 0)

	for _, state := range cb.states {
		if !state.backend.Enabled {
			continue
		}

		// Check if in rate limit cooldown
		if !state.last429Time.IsZero() && now.Sub(state.last429Time) < cooldown {
			rateLimited = append(rateLimited, state)
		} else {
			normal = append(normal, state)
		}
	}

	// Normal backends first, then rate-limited ones
	result := append(normal, rateLimited...)
	return result
}

// CircuitBreakerStateInfo represents circuit breaker state information
type CircuitBreakerStateInfo struct {
	State               string
	ConsecutiveFailures int
	LastFailureTime     time.Time
	LastError           string
}

// RateLimitStateInfo represents rate limit state information
type RateLimitStateInfo struct {
	CooldownUntil *time.Time
	RetryAfter    int
}

// GetBackendState returns the circuit breaker state for a backend by name
func (cb *CircuitBreaker) GetBackendState(name string) CircuitBreakerStateInfo {
	cb.stateMu.RLock()
	defer cb.stateMu.RUnlock()

	for _, state := range cb.states {
		if state.backend.Name == name {
			stateStr := "closed"
			if state.circuitOpen {
				timeout := time.Duration(cb.config.Failover.CircuitBreaker.OpenTimeoutSeconds) * time.Second
				if time.Since(state.lastFailTime) >= timeout {
					stateStr = "half-open"
				} else {
					stateStr = "open"
				}
			}

			return CircuitBreakerStateInfo{
				State:               stateStr,
				ConsecutiveFailures: state.consecutiveFails,
				LastFailureTime:     state.lastFailTime,
				LastError:           state.lastError,
			}
		}
	}

	return CircuitBreakerStateInfo{State: "unknown"}
}

// GetRateLimitState returns the rate limit state for a backend by name
func (cb *CircuitBreaker) GetRateLimitState(name string) RateLimitStateInfo {
	cb.stateMu.RLock()
	defer cb.stateMu.RUnlock()

	for _, state := range cb.states {
		if state.backend.Name == name {
			var cooldownUntil *time.Time
			retryAfter := 0

			if !state.last429Time.IsZero() {
				cooldown := time.Duration(cb.config.Failover.RateLimit.CooldownSeconds) * time.Second
				until := state.last429Time.Add(cooldown)
				if time.Now().Before(until) {
					cooldownUntil = &until
					retryAfter = int(time.Until(until).Seconds())
				}
			}

			return RateLimitStateInfo{
				CooldownUntil: cooldownUntil,
				RetryAfter:    retryAfter,
			}
		}
	}

	return RateLimitStateInfo{}
}

// OnBackendEnabled is called when a backend is enabled via management API
func (cb *CircuitBreaker) OnBackendEnabled(name string) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	for _, state := range cb.states {
		if state.backend.Name == name {
			state.backend.Enabled = true
			// Reset circuit breaker state when enabling
			state.consecutiveFails = 0
			state.circuitOpen = false
			state.halfOpenTries = 0
			log.Printf("[后端启用] %s - 已启用并重置熔断状态", name)
			return
		}
	}
}

// OnBackendDisabled is called when a backend is disabled via management API
func (cb *CircuitBreaker) OnBackendDisabled(name string) {
	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	for _, state := range cb.states {
		if state.backend.Name == name {
			state.backend.Enabled = false
			log.Printf("[后端禁用] %s - 已禁用", name)
			return
		}
	}
}
