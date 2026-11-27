package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcp"
)

// mcp-08: a target database the fleet cannot serve is "retry later"
// (-32010), never an internal error.
func TestMCPTargetPoolUnavailableIsTypedUnavailable(t *testing.T) {
	ctx := mcp.WithDatabase(context.Background(), "orders")
	access := &fleetMCPAccess{}
	if _, err := access.GetPolicy(ctx, mcp.PolicyRequest{}); !errors.Is(err,
		mcp.ErrIntentUnavailable) {
		t.Fatalf("named database without an instance: err = %v, want ErrIntentUnavailable",
			err)
	}
	id := int64(9)
	if _, err := access.GetLedger(context.Background(), mcp.LedgerRequest{}); err == nil {
		t.Fatal("no instance and no fallback: want an error")
	} else if !errors.Is(err, mcp.ErrIntentUnavailable) {
		t.Fatalf("no fallback: err = %v, want ErrIntentUnavailable", err)
	}
	if _, err := access.GetPolicy(context.Background(),
		mcp.PolicyRequest{DatabaseID: &id}); !errors.Is(err, mcp.ErrIntentUnavailable) {
		t.Fatalf("unknown database_id: err = %v, want ErrIntentUnavailable", err)
	}
}

// get_value without a fleet is the same "retry later", never an internal
// error.
func TestMCPValueWithoutFleetIsTypedUnavailable(t *testing.T) {
	var unwired *fleetMCPAccess
	for _, access := range []*fleetMCPAccess{unwired, {}} {
		if _, err := access.GetValue(context.Background()); !errors.Is(err,
			mcp.ErrIntentUnavailable) {
			t.Fatalf("no fleet: err = %v, want ErrIntentUnavailable", err)
		}
	}
}
