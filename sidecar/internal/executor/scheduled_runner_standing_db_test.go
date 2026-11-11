package executor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// policy-04 review: a schedule the gate withholds at its opening returns
// to pending and asks for a new approval. It must leave no standing
// approval behind: an approved recommendation is a durable operator
// approval the executor cycle runs on its own (runOperatorApproval), so it
// would run later although the approver was told it needs approving again.

func TestWithheldScheduleLeavesNoStandingApproval(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	var stopped atomic.Bool
	stopped.Store(true)
	fx.exec.WithEmergencyStopCheck(func(context.Context) bool { return stopped.Load() })
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d under an emergency stop", n)
	}
	if row := fx.row(t); row.Status != "pending" || row.DecidedBy != nil {
		t.Fatalf("row = %+v, want pending and undecided", row)
	}
	head, err := fx.recs.Get(fx.ctx, fx.rec.ID)
	if err != nil {
		t.Fatalf("get recommendation: %v", err)
	}
	if head.State != recommendation.StateProposed {
		t.Fatalf("recommendation state = %q, want %q: a withheld schedule must not "+
			"leave it approved", head.State, recommendation.StateProposed)
	}
	// The stop is lifted inside the same window: the executor's own cycle
	// must not run the returned approval.
	stopped.Store(false)
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 0 {
		t.Fatalf("the executor cycle ran the returned approval %d times", n)
	}
	if row := fx.row(t); row.Status != "pending" {
		t.Fatalf("row = %+v after the cycle, want still pending", row)
	}
}

// The same holds for a window closing under the start margin at the
// moment the runner looks: it is withheld, never left approved.
func TestClosingWindowScheduleLeavesNoStandingApproval(t *testing.T) {
	doc := staffedUTC()
	doc.WindowStartMarginMinutes = 30
	fx := newScheduleFixture(t, doc)
	fx.schedule(t)
	fx.open(t)
	at := fx.clock.now()                               // Thursday 02:00
	fx.clock.set(at.Add(2*time.Hour + 45*time.Minute)) // 04:45: 15 minutes left
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d with 15 minutes left under a 30-minute margin", n)
	}
	head, err := fx.recs.Get(fx.ctx, fx.rec.ID)
	if err != nil {
		t.Fatalf("get recommendation: %v", err)
	}
	if head.State != recommendation.StateProposed || fx.row(t).Status != "pending" {
		t.Fatalf("recommendation %q, row %q; want proposed and pending", head.State,
			fx.row(t).Status)
	}
}
