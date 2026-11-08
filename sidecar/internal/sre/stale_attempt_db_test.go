package sre

import (
	"testing"
	"time"
)

// State transition (sre-01 with sre-02): a resumed run on hour-old
// evidence ends expired/stale_evidence; its incident is still open, so
// the trigger's next poll starts attempt 2, which supersedes it and
// collects fresh evidence instead of reusing the old steps.
func TestStaleResume_NextPollStartsAFreshAttempt(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	old := orphanLockRun(t, ctx, st, pool, c, "att-stale", time.Now().Add(-time.Hour))
	if err := c.Investigate(ctx, old.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertStaleExpired(t, ctx, st, scope, old.ID, time.Hour)
	next, created, err := c.Start(ctx, lockTrigger("att-stale"))
	if err != nil || !created || next.ID == old.ID || next.Attempt != 2 ||
		next.Supersedes != old.ID {
		t.Fatalf("poll of the open incident = %s attempt %d supersedes %q created=%v err=%v",
			next.ID, next.Attempt, next.Supersedes, created, err)
	}
	if err := c.Investigate(ctx, next.ID); err != nil {
		t.Fatalf("investigate attempt 2: %v", err)
	}
	if runner.total() == 0 {
		t.Fatal("attempt 2 ran no probe; it must collect fresh evidence")
	}
	got, err := st.Get(ctx, scope, next.ID)
	if err != nil || got.State != StateConcluded || got.Summary.Root != "idle_in_tx_holder" ||
		got.Summary.EvidenceAgeS > int64(MaxEvidenceSpread/time.Second) {
		t.Fatalf("attempt 2 = %s %q evidence age %d s (%v)", got.State, got.Summary.Root,
			got.Summary.EvidenceAgeS, err)
	}
	prior, err := st.Get(ctx, scope, old.ID)
	if err != nil || prior.State != StateExpired || prior.SupersededBy != next.ID {
		t.Fatalf("attempt 1 = %s superseded by %q (%v)", prior.State, prior.SupersededBy, err)
	}
	again, created, err := c.Start(ctx, lockTrigger("att-stale"))
	if err != nil || created || again.ID != next.ID {
		t.Fatalf("poll after attempt 2 concluded = %s created=%v err=%v", again.ID,
			created, err)
	}
}
