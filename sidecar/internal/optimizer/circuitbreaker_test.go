package optimizer

import (
	"testing"
	"time"
)

func TestNewCircuitBreaker_ShouldSkipFalse(t *testing.T) {
	cb := NewCircuitBreaker()
	if cb.ShouldSkip("public", "users") {
		t.Error("new breaker should not skip any table")
	}
}

func TestRecordFailure(t *testing.T) {
	tests := []struct {
		name      string
		failures  int
		wantSkip  bool
		wantState CircuitState
	}{
		{"1 failure stays closed", 1, false, CircuitClosed},
		{"2 failures stays closed", 2, false, CircuitClosed},
		{"3 failures opens circuit", 3, true, CircuitOpen},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := NewCircuitBreaker()
			for i := 0; i < tt.failures; i++ {
				cb.RecordFailure("public", "orders")
			}
			if got := cb.ShouldSkip("public", "orders"); got != tt.wantSkip {
				t.Errorf("ShouldSkip = %v, want %v", got, tt.wantSkip)
			}
			if got := cb.GetState("public", "orders"); got != tt.wantState {
				t.Errorf("GetState = %v, want %v", got, tt.wantState)
			}
		})
	}
}

func TestRecordSuccess_ResetsAfterFailures(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.RecordFailure("public", "items")
	cb.RecordFailure("public", "items")
	cb.RecordSuccess("public", "items")

	if cb.ShouldSkip("public", "items") {
		t.Error("success should reset circuit, ShouldSkip should be false")
	}
	if got := cb.GetState("public", "items"); got != CircuitClosed {
		t.Errorf("GetState = %v, want %v", got, CircuitClosed)
	}
}

func TestGetState(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*CircuitBreaker)
		wantState CircuitState
	}{
		{
			"unknown table returns closed",
			func(cb *CircuitBreaker) {},
			CircuitClosed,
		},
		{
			"3 failures returns open",
			func(cb *CircuitBreaker) {
				cb.RecordFailure("public", "t")
				cb.RecordFailure("public", "t")
				cb.RecordFailure("public", "t")
			},
			CircuitOpen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := NewCircuitBreaker()
			tt.setup(cb)
			if got := cb.GetState("public", "t"); got != tt.wantState {
				t.Errorf("GetState = %v, want %v", got, tt.wantState)
			}
		})
	}
}

func TestCooldownTransition(t *testing.T) {
	cb := NewCircuitBreaker()

	// Open the circuit
	cb.RecordFailure("public", "big")
	cb.RecordFailure("public", "big")
	cb.RecordFailure("public", "big")

	if !cb.ShouldSkip("public", "big") {
		t.Fatal("circuit should be open after 3 failures")
	}

	// Simulate cooldown expiry by backdating LastFailure
	cb.mu.Lock()
	tc := cb.circuits[tableKey("public", "big")]
	tc.LastFailure = time.Now().Add(-25 * time.Hour) // > 1-day cooldown
	cb.mu.Unlock()

	// After cooldown, ShouldSkip transitions to half_open and returns false
	if cb.ShouldSkip("public", "big") {
		t.Error("should not skip after cooldown expires")
	}
	if got := cb.GetState("public", "big"); got != CircuitHalfOpen {
		t.Errorf("state after cooldown = %v, want %v", got, CircuitHalfOpen)
	}
}

func TestHalfOpenFailureEscalateCooldown(t *testing.T) {
	cb := NewCircuitBreaker()

	// Open circuit (1-day cooldown)
	cb.RecordFailure("public", "x")
	cb.RecordFailure("public", "x")
	cb.RecordFailure("public", "x")

	// Expire cooldown to reach half_open
	cb.mu.Lock()
	tc := cb.circuits[tableKey("public", "x")]
	tc.LastFailure = time.Now().Add(-25 * time.Hour)
	cb.mu.Unlock()
	cb.ShouldSkip("public", "x") // triggers half_open

	// Failure in half_open → re-open with 7-day cooldown
	cb.RecordFailure("public", "x")
	if got := cb.GetState("public", "x"); got != CircuitOpen {
		t.Errorf("state = %v, want %v", got, CircuitOpen)
	}

	cb.mu.Lock()
	tc = cb.circuits[tableKey("public", "x")]
	if tc.CooldownDays != 7 {
		t.Errorf("CooldownDays = %d, want 7", tc.CooldownDays)
	}
	cb.mu.Unlock()
}
