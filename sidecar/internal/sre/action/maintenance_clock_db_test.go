package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// ageBy moves the proposal's last change to the database clock minus d.
func (h *maintenanceHarness) ageBy(t *testing.T, id sre.UUID, d time.Duration) {
	t.Helper()
	_, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals
		SET updated_at=clock_timestamp() - make_interval(secs => $2) WHERE id=$1`,
		string(id), d.Seconds())
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceReconcileAgesByTheDatabaseClock(t *testing.T) {
	h := newMaintenanceHarness(t)
	if h.m.Now != nil {
		t.Fatal("the production sweeper must not age proposals by the host clock")
	}
	p := h.request(t)
	h.strand(t, p.ID, "executing")
	h.ageBy(t, p.ID, h.m.strandedAfter()-30*time.Second)
	if err := h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "executing" || h.settleEvents(t, p.ID) != 0 {
		t.Fatalf("younger than the bound on the database clock was swept: %+v", got)
	}
	// The cutoff is read from the database clock, which stamps updated_at.
	cutoff, err := h.m.strandedCutoff(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dbNow time.Time
	if err = h.pool.QueryRow(h.ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	if lag := dbNow.Add(-h.m.strandedAfter()).Sub(cutoff); lag < 0 || lag > 10*time.Second {
		t.Fatalf("cutoff %v is not the database clock minus the bound (lag %v)", cutoff, lag)
	}
	h.pastBound(t, p.ID)
	if err = h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "failed" || h.settleEvents(t, p.ID) != 1 {
		t.Fatalf("past the bound on the database clock was not swept: %+v", got)
	}
}

// A proposal written after the sweeper listed it (a live retry) is not
// settled from the stale listing.
func TestMaintenanceReconcileSkipsAProposalTouchedSinceListing(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	h.strand(t, p.ID, "executing")
	listedAt := time.Now().Add(-time.Hour)
	if err := h.m.reconcileOne(h.ctx, p.ID, listedAt); !errors.Is(err, errSettled) {
		t.Fatalf("fresh proposal reconciled from a stale cutoff: %v", err)
	}
	if got := h.state(t, p.ID); got.State != "executing" || h.settleEvents(t, p.ID) != 0 {
		t.Fatalf("fresh proposal changed: %+v", got)
	}
}

// A live run holds its proposal for at most one bounded slot wait and one
// detached executor run, so the bound must cover both with margin.
func TestMaintenanceRunBoundsTheSlotWait(t *testing.T) {
	h := newMaintenanceHarness(t)
	apply := h.exec.ApplyTimeout()
	began := time.Now()
	ctx, cancel := h.m.boundedRun(context.Background())
	defer cancel()
	ended := time.Now()
	deadline, ok := ctx.Deadline()
	// The deadline is one apply timeout after a moment inside [began, ended].
	if !ok || deadline.Before(began.Add(apply)) || deadline.After(ended.Add(apply)) {
		t.Fatalf("slot wait deadline %v (set %v) not one apply timeout %v", deadline, ok, apply)
	}
	if h.m.strandedAfter() < 2*apply+30*time.Second {
		t.Fatalf("stranded bound %v does not cover two apply timeouts %v", h.m.strandedAfter(),
			apply)
	}
}
