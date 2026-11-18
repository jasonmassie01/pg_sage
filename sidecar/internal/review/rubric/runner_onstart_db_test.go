package rubric

import (
	"context"
	"errors"
	"testing"
)

// review-04a: OnStart sees the recorded run before it is graded, and an
// OnStart error fails the run rather than leaving it running.

func TestRunnerDB_OnStartSeesTheRunningRow(t *testing.T) {
	pool, ctx := livePool(t)
	var seen int64
	var status string
	run := &Runner{Store: NewStore(pool), Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: V1(), Database: "orders", OnStart: func(ctx context.Context, id int64) error {
			seen = id
			return pool.QueryRow(ctx, `SELECT status FROM sage.review_run WHERE id = $1`,
				id).Scan(&status)
		}}
	out, err := run.Run(ctx, KindReview, []string{"pre_upgrade"})
	if err != nil || seen == 0 || seen != out.RunID || status != "running" {
		t.Fatalf("run %+v %v, OnStart saw %d %q, want the running row", out, err, seen, status)
	}
}

func TestRunnerDB_OnStartErrorFailsTheRun(t *testing.T) {
	pool, ctx := livePool(t)
	boom := errors.New("claim lost")
	run := &Runner{Store: NewStore(pool), Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: V1(), Database: "orders",
		OnStart: func(context.Context, int64) error { return boom }}
	out, err := run.Run(ctx, KindReview, []string{"calendar"})
	if !errors.Is(err, boom) || out.RunID == 0 {
		t.Fatalf("run %+v %v, want the OnStart error", out, err)
	}
	var status, msg string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM sage.review_run WHERE id = $1`,
		out.RunID).Scan(&status, &msg); err != nil || status != "failed" ||
		msg != "claim lost" {
		t.Fatalf("row %q %q (%v), want failed with the error", status, msg, err)
	}
}
