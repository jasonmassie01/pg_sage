package optimizer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

func TestAuditAdmissionRejectionDoesNotTripTableCircuit(t *testing.T) {
	breaker := NewCircuitBreaker()
	err := fmt.Errorf("llm chat: %w", llm.ErrRequestCooldown)
	for range 10 {
		if shouldTripTableCircuit(err) {
			breaker.RecordFailure("public", "orders")
		}
	}
	if state := breaker.GetState("public", "orders"); state != CircuitClosed {
		t.Fatalf("admission-only cooldown changed circuit to %s", state)
	}
}

func TestAuditProviderFailuresStillTripTableCircuit(t *testing.T) {
	breaker := NewCircuitBreaker()
	providerErr := errors.New("provider returned 503")
	for range 3 {
		if shouldTripTableCircuit(providerErr) {
			breaker.RecordFailure("public", "orders")
		}
	}
	if state := breaker.GetState("public", "orders"); state != CircuitOpen {
		t.Fatalf("provider failures left circuit %s, want open", state)
	}
}

func TestAuditWrappedAdmissionErrorsRemainClassified(t *testing.T) {
	err := fmt.Errorf("optimizer: %w", fmt.Errorf("llm chat: %w", llm.ErrRequestCooldown))
	if shouldTripTableCircuit(err) {
		t.Fatal("wrapped admission error was classified as table failure")
	}
}

func TestAuditNilErrorDoesNotTripTableCircuit(t *testing.T) {
	if shouldTripTableCircuit(nil) {
		t.Fatal("nil error was classified as table failure")
	}
}

// Concurrent state mutation is already covered by circuitbreaker_test.go.
// This file tests the boundary between LLM admission and that synchronized
// state. No database integration applies because classification is pure.
