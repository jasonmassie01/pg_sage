package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

func horizonAt(xidAge, mxidAge int64) freezeHorizon {
	return freezeHorizon{xidAge: xidAge, xidMax: 200_000_000,
		mxidAge: mxidAge, mxidMax: 400_000_000}
}

// Credit is earned only by a table that was red before the action and is
// green after it (spec F3: "crosses red and an action returns it below
// amber").
func TestFreezeNearMissRequiresRedBeforeAndGreenAfter(t *testing.T) {
	thresholds := freeze.BufferThresholds(25)
	tests := []struct {
		name          string
		before, after freezeHorizon
		want          bool
	}{
		{"red to green", horizonAt(160_000_000, 0), horizonAt(0, 0), true},
		{"red at the boundary to green", horizonAt(150_000_000, 0), horizonAt(99_999_999, 0), true},
		{"red to amber", horizonAt(160_000_000, 0), horizonAt(100_000_000, 0), false},
		{"red stays red", horizonAt(160_000_000, 0), horizonAt(155_000_000, 0), false},
		{"amber to green", horizonAt(149_999_999, 0), horizonAt(0, 0), false},
		{"green to green", horizonAt(10, 0), horizonAt(0, 0), false},
		{"multixact red to green", horizonAt(0, 300_000_000), horizonAt(0, 0), true},
		{"xid fixed but multixact still amber", horizonAt(160_000_000, 250_000_000),
			horizonAt(0, 250_000_000), false},
		{"zero baseline is not red", freezeHorizon{}, horizonAt(0, 0), false},
		{"unmeasured after is not green", horizonAt(160_000_000, 0), freezeHorizon{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := freezeNearMiss(&tt.before, tt.after, thresholds); got != tt.want {
				t.Fatalf("freezeNearMiss = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFreezeNearMissWithoutBaselineEarnsNothing(t *testing.T) {
	if freezeNearMiss(nil, horizonAt(0, 0), freeze.BufferThresholds(25)) {
		t.Fatal("a missing baseline must not earn incident credit")
	}
}

// The red buffer comes from configuration: the same table is a near miss
// under the default 25% buffer and only amber under a 10% buffer.
func TestFreezeNearMissHonorsConfiguredBuffer(t *testing.T) {
	before, after := horizonAt(160_000_000, 0), horizonAt(0, 0)
	if !freezeNearMiss(&before, after, freeze.BufferThresholds(25)) {
		t.Fatal("160M of 200M is red under a 25% buffer")
	}
	if freezeNearMiss(&before, after, freeze.BufferThresholds(10)) {
		t.Fatal("160M of 200M is only amber under a 10% buffer")
	}
}

// No concurrent access tests: freezeNearMiss is pure. The durable write is
// raced in internal/value (TestCreditAvoidedIncidentConcurrentCallersCreditOnce).
