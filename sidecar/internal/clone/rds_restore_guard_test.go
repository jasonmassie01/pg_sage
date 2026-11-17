package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05d review: a restore is refused before anything is made when its
// caller ends before the restore deadline or the source changed since it
// was read, and a restore RDS accepted but does not describe stays
// uncertain for the reaper.
// No concurrent access tests here: each check is one sequential Restore
// (TestRDSRestore_TwoRestoresGetTwoNames covers concurrent restores).

// Boundary: a caller that ends before the restore deadline (a bounded MCP
// call) is refused before credentials are loaded or RDS is asked; one
// that outlasts it restores.
func TestRDSRestore_ACallerEndingBeforeTheDeadlineCreatesNothing(t *testing.T) {
	f := newRDSFake(t)
	loads := 0
	p := newRDSProvider(t, f, func(c *clone.RDSRestoreConfig) {
		c.API = func(context.Context) (clone.RDSRestoreClient, error) {
			loads++
			return f, nil
		}
	})
	short, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	before := loads
	_, err := p.Create(short, clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrCloneUnavailable) || errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "clone.restore_deadline_minutes") {
		t.Fatalf("err = %v, want unavailable naming clone.restore_deadline_minutes", err)
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q, want none", got)
	}
	// LatestSnapshot (a read) loads credentials once; Restore never does.
	if loads-before != 1 {
		t.Fatalf("credential loads = %d, want only the snapshot read's", loads-before)
	}
	long, cancelLong := context.WithTimeout(context.Background(), 2*time.Hour+time.Minute)
	defer cancelLong()
	c, err := p.Create(long, clone.CloneSpec{ResourceName: rdsName})
	if err != nil || c.ID != rdsName {
		t.Fatalf("create with a caller past the deadline = %+v, %v", c, err)
	}
}

// Invalid input: a snapshot of another instance than the one now
// monitored creates nothing.
func TestRDSRestore_ASourceChangedSinceTheSnapshotCreatesNothing(t *testing.T) {
	f := newRDSFake(t)
	a := newRDSAPI(t, f, nil)
	_, err := a.Restore(context.Background(), clone.Snapshot{ID: "orders",
		CreatedAt: rdsNow.Add(-time.Minute)}, clone.CloneSpec{ResourceName: rdsName})
	if err == nil || !strings.Contains(err.Error(), "not orders") {
		t.Fatalf("err = %v, want the changed source refused", err)
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q, want none", got)
	}
}

// Error propagation: a restore RDS accepted but never describes is left
// behind (create_uncertain), not closed as a clean failure.
func TestRDSRestore_AnAcceptedRestoreNotDescribedStaysUncertain(t *testing.T) {
	f := newRDSFake(t)
	f.unlisted = true
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !errors.Is(err, clone.ErrRDSNotFound) {
		t.Fatalf("err = %v, want left behind after a not-found poll", err)
	}
	if got := f.mutating(); len(got) != 1 || !strings.HasPrefix(got[0], "restore ") {
		t.Fatalf("mutating calls %q, want the restore only", got)
	}
}
