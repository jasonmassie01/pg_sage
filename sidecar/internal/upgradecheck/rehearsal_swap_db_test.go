package upgradecheck

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/clone"
)

// swappingRehearser answers with another clone than the one it was
// given: the rehearsal must refuse it and destroy only its own clone.
type swappingRehearser struct{ dsn string }

func (s swappingRehearser) Upgrade(_ context.Context, cl clone.Clone,
	_ int) (clone.Clone, error) {
	return clone.Clone{DSN: s.dsn, ID: cl.ID + "-elsewhere"}, nil
}

func TestRehearsalDB_UpgraderAnsweringAnotherCloneIsRefused(t *testing.T) {
	f := newRehearsalFixture(t)
	rep, err := runFixture(t, f, swappingRehearser{dsn: f.targetDSN})
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("Rehearse = %v, want ErrUpgradeRefused for a swapped clone", err)
	}
	want := []string{RehearsalRequested, RehearsalCloned, RehearsalFailed,
		RehearsalDestroyed}
	if got := stateNames(rep); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if got := f.inner.destroyedIDs(); !slices.Equal(got, []string{rep.CloneID}) {
		t.Fatalf("destroyed %v, want only the rehearsal clone %s", got, rep.CloneID)
	}
	if rep.Error == "" || len(rep.Changed) != 0 || !rep.AnalyzeAt.IsZero() {
		t.Fatalf("report after a swapped clone: %+v", rep)
	}
}
