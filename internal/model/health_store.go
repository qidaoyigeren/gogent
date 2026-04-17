package model

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CircuitState represents the three states of a circuit breaker.
type CircuitState int

const (
	StateClosed   CircuitState = iota // healthy
	StateOpen                         // tripped
	StateHalfOpen                     // probing
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

type modelHealth struct {
	state        CircuitState
	failureCount int
	openedAt     time.Time
	// halfOpenInFlight ensures only a single probe request is allowed through
	// when the circuit is HALF_OPEN, matching Java ModelHealthStore.allowCall behavior.
	halfOpenInFlight atomic.Bool
}

// HealthStore manages per-model circuit breaker state.
type HealthStore struct {
	mu               sync.Mutex
	models           map[string]*modelHealth
	failureThreshold int
	openDuration     time.Duration
}

// NewHealthStore creates a HealthStore.
func NewHealthStore(failureThreshold int, openDurationMs int64) *HealthStore {
	return &HealthStore{
		models:           make(map[string]*modelHealth),
		failureThreshold: failureThreshold,
		openDuration:     time.Duration(openDurationMs) * time.Millisecond,
	}
}

func (h *HealthStore) getOrCreate(modelID string) *modelHealth {
	m, ok := h.models[modelID]
	if !ok {
		m = &modelHealth{state: StateClosed}
		h.models[modelID] = m
	}
	return m
}

// IsAvailable checks if a model is available.
// In HALF_OPEN state, only the first caller wins (single-flight probe), matching Java behavior.
func (h *HealthStore) IsAvailable(modelID string) bool {
	h.mu.Lock()
	m := h.getOrCreate(modelID)
	state := m.state
	openedAt := m.openedAt

	switch state {
	case StateClosed:
		h.mu.Unlock()
		return true

	case StateOpen:
		if time.Since(openedAt) >= h.openDuration {
			m.state = StateHalfOpen
			slog.Info("circuit breaker half-open", "model", modelID)
			// Fall through to HALF_OPEN handling below
		} else {
			h.mu.Unlock()
			return false
		}
		fallthrough

	case StateHalfOpen:
		h.mu.Unlock()
		// Only one goroutine may probe at a time (CAS false→true).
		if m.halfOpenInFlight.CompareAndSwap(false, true) {
			return true
		}
		return false
	}

	h.mu.Unlock()
	return false
}

// RecordSuccess records a successful call, resets the circuit breaker.
func (h *HealthStore) RecordSuccess(modelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	m := h.getOrCreate(modelID)
	if m.state != StateClosed {
		slog.Info("circuit breaker closed", "model", modelID, "prev", m.state.String())
	}
	m.state = StateClosed
	m.failureCount = 0
	m.halfOpenInFlight.Store(false)
}

// RecordFailure records a failed call, may trip the circuit breaker.
func (h *HealthStore) RecordFailure(modelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	m := h.getOrCreate(modelID)
	m.failureCount++
	m.halfOpenInFlight.Store(false)

	if m.state == StateHalfOpen || m.failureCount >= h.failureThreshold {
		m.state = StateOpen
		m.openedAt = time.Now()
		slog.Warn("circuit breaker opened", "model", modelID, "failures", m.failureCount)
	}
}
