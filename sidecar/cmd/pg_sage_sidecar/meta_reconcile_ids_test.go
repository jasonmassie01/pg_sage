package main

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01d review: a reconcile pass reads only the members that get a
// runtime and the runtimes still running, never each observe-only row.
// No concurrent access test: reconcileIDs is a pure function of its inputs.
func TestReconcileIDs_SkipsMembersWithoutARuntime(t *testing.T) {
	records := []store.DatabaseRecord{
		{ID: 1, Enabled: true, Tier: store.TierFull},
		{ID: 2, Enabled: true, Tier: store.TierObserveOnly},
		{ID: 3, Enabled: false, Tier: store.TierFull},
		{ID: 4, Enabled: true, Tier: "light"}, // unknown tier: no runtime
		{ID: 5, Enabled: false, Tier: store.TierObserveOnly},
		{ID: 6, Enabled: true, Tier: store.TierObserveOnly},
	}
	// 3 was disabled and 6 demoted while their runtimes still run: the pass
	// must look at them to remove the runtimes.
	instances := map[string]*fleet.DatabaseInstance{
		"three": {DatabaseID: 3}, "six": {DatabaseID: 6}, "yaml": {DatabaseID: 0},
	}
	got := reconcileIDs(records, instances)
	if want := []int{1, 3, 6}; !slices.Equal(got, want) {
		t.Fatalf("reconcileIDs = %v, want %v", got, want)
	}
}

func TestReconcileIDs_Empty(t *testing.T) {
	if got := reconcileIDs(nil, nil); len(got) != 0 {
		t.Fatalf("reconcileIDs(nil, nil) = %v, want none", got)
	}
	observeOnly := make([]store.DatabaseRecord, 2000)
	for i := range observeOnly {
		observeOnly[i] = store.DatabaseRecord{ID: i + 1, Enabled: true,
			Tier: store.TierObserveOnly}
	}
	if got := reconcileIDs(observeOnly, nil); len(got) != 0 {
		t.Fatalf("2,000 observe-only members: %d ids to read, want none", len(got))
	}
}
