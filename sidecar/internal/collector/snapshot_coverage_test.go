package collector

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// Change-only catalog storage (roadmap phase 3): every category the
// collector persists has an explicit storage decision in the snapshot
// store, change-only or full with a reason. A category added here without
// one fails this test instead of silently growing sage.snapshots in full.
func TestEveryPersistedCategoryHasAStorageDecision(t *testing.T) {
	rows, err := snapshotRows(&Snapshot{})
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	if len(rows) < 11 {
		t.Fatalf("persisted %d categories, want every one of the 11", len(rows))
	}
	delta := 0
	for _, r := range rows {
		isDelta, reason := snapstore.Coverage(r.Category)
		if !isDelta && reason == "" {
			t.Errorf("category %q has no storage decision in snapstore", r.Category)
		}
		if isDelta {
			delta++
		}
	}
	if delta < 8 {
		t.Fatalf("%d categories are change-only, want the 8 catalog categories", delta)
	}
}
