package trigger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// reapingReviewer is sqlReviewer behind the rubric runner's reaper: it
// fails the database's runs left running for longer than the test lease
// before it starts its own, as rubric.Runner.Run does.
type reapingReviewer struct{ *sqlReviewer }

func (r reapingReviewer) Review(ctx context.Context, run Run) (int64, error) {
	if _, err := r.pool.Exec(ctx, `UPDATE sage.review_run SET status = 'failed',
		finished_at = now(), error = 'reaped' WHERE database_name = $1
		AND status = 'running' AND started_at < now() - interval '10 minutes'`,
		run.Database); err != nil {
		return 0, err
	}
	return r.sqlReviewer.Review(ctx, run)
}

// A leader killed mid-run (no chance to fail its run) leaves the run
// running. While the run is younger than the lease its triggers wait; once
// it is older, the next leader reruns them at once instead of waiting for
// some later grade to reap the dead run.
func TestTriggerDB_RunLeftRunningByADeadLeaderIsRerunAfterTheLease(t *testing.T) {
	pool, ctx := livePool(t, "stale")
	s := NewStore(pool)
	a := fire(t, ctx, s, "shop", KindPreUpgrade, t0)
	if _, err := s.Claim(ctx, "shop", []int64{a}, "dead", t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		triggers, rubric_version) VALUES ('shop', 'review', '{pre_upgrade}', 'v')
		RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, []int64{a}, run, "dead"); err != nil {
		t.Fatal(err)
	}
	rv := reapingReviewer{&sqlReviewer{pool: pool}}
	sched := scheduler(s, "shop", "next", rv, &clock{now: t0.Add(11 * time.Minute)})
	res, err := sched.Tick(ctx)
	if err != nil || res.RunID != 0 || res.Resumed || rv.count() != 0 {
		t.Fatalf("young running run: %+v %v, %d reviews; want the trigger to wait", res, err,
			rv.count())
	}
	if st := states(t, ctx, pool)[a]; st != fmt.Sprintf("fired run=%d into=0", run) {
		t.Fatalf("waiting trigger state %q", st)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.review_run
		SET started_at = now() - interval '11 minutes' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	res, err = sched.Tick(ctx)
	if err != nil || res.RunID == 0 || res.RunID == run || rv.count() != 1 {
		t.Fatalf("stale running run: %+v %v, %d reviews; want one new run", res, err,
			rv.count())
	}
	if st := states(t, ctx, pool)[a]; st != fmt.Sprintf("consumed run=%d into=0", res.RunID) {
		t.Fatalf("rerun trigger state %q", st)
	}
	runs := reviewRuns(t, ctx, pool, "shop")
	if len(runs) != 2 || runs[0] != "pre_upgrade:failed" || runs[1] != "pre_upgrade:done" {
		t.Fatalf("runs %v, want the dead run failed and one done", runs)
	}
}

// The stale bound is the database's clock: a zero bound never reports a
// running run stale, and a missing run is "".
func TestTriggerDB_RunStatusStaleBound(t *testing.T) {
	pool, ctx := livePool(t, "stalebound")
	s := NewStore(pool)
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		triggers, rubric_version, started_at) VALUES ('shop', 'review', '{calendar}', 'v',
		now() - interval '1 hour') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		bound time.Duration
		want  string
	}{{0, "running"}, {2 * time.Hour, "running"}, {30 * time.Minute, RunStale}} {
		if st, err := s.RunStatus(ctx, run, c.bound); err != nil || st != c.want {
			t.Fatalf("RunStatus(bound %s) = %q %v, want %q", c.bound, st, err, c.want)
		}
	}
	if st, err := s.RunStatus(ctx, run+1000, time.Minute); err != nil || st != "" {
		t.Fatalf("missing run = %q %v, want \"\"", st, err)
	}
}
