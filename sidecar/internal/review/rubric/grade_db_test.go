package rubric

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// review-01a on a real server: the run store, its one-running-row rule,
// its state transitions, and the runner's error propagation.

func spec1(db string) RunSpec {
	return RunSpec{Database: db, Kind: KindGrade, Triggers: []string{"test"},
		RubricVersion: Version}
}

func TestStoreDB_TwoRunsForOneDatabaseGiveOneRunningRow(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	var wg sync.WaitGroup
	ids := make([]int64, 4)
	errs := make([]error, 4)
	for i, db := range []string{"alpha", "alpha", "beta", "gamma"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = s.Start(ctx, spec1(db))
		}()
	}
	wg.Wait()
	alphaOK := (errs[0] == nil) != (errs[1] == nil)
	alphaBusy := errors.Is(errs[0], ErrRunning) || errors.Is(errs[1], ErrRunning)
	if !alphaOK || !alphaBusy {
		t.Fatalf("alpha starts = %v / %v, want one id and one ErrRunning", errs[0], errs[1])
	}
	if errs[2] != nil || errs[3] != nil || ids[2] == 0 || ids[3] == 0 {
		t.Fatalf("beta, gamma = %v %v (%d %d), want both running", errs[2], errs[3], ids[2],
			ids[3])
	}
	var running int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_run
		WHERE status = 'running'`).Scan(&running); err != nil || running != 3 {
		t.Fatalf("running rows = %d %v, want 3", running, err)
	}
}

func TestStoreDB_StateTransitions(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	id, err := s.Start(ctx, spec1("orders"))
	if err != nil {
		t.Fatal(err)
	}
	r, rs := happyRubric()
	g := Compute(r, rs)
	if err := s.Finish(ctx, id, RunDone, &g, ""); err != nil {
		t.Fatalf("finish done: %v", err)
	}
	if err := s.Finish(ctx, id, RunFailed, nil, "late"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a done run moved again: %v", err)
	}
	if n, err := s.Reap(ctx, "orders", 0); err != nil || n != 0 {
		t.Fatalf("reap of a done run = %d %v, want 0", n, err)
	}
	if err := s.Finish(ctx, id, RunRunning, nil, ""); err == nil {
		t.Fatal("finish to running was accepted")
	}
	runs, err := s.Latest(ctx, "orders", 5)
	if err != nil || len(runs) != 1 || runs[0].Status != RunDone ||
		runs[0].Grade.Overall != "D" || runs[0].FinishedAt == nil ||
		len(runs[0].Grade.Dimensions) != 5 {
		t.Fatalf("latest = %+v %v, want the done run with its 5 dimensions", runs, err)
	}
	if d := dimOf(t, runs[0].Grade, "d5"); d.Letter != "" || !near(d.Score, 50) ||
		len(d.Checks) != 2 || d.Checks[0].Status != StatusUnknown {
		t.Fatalf("stored d5 = %+v, want unknown at 50 with its checks", d)
	}
	for _, st := range []RunStatus{RunPartial, RunFailed} {
		id, err := s.Start(ctx, spec1("orders"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, id, st, &g, "firstlook: boom"); err != nil {
			t.Fatalf("finish %s: %v", st, err)
		}
	}
}

func TestStoreDB_ReaperFailsAStaleRunningRow(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	stale, err := s.Start(ctx, spec1("orders"))
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, pool, `UPDATE sage.review_run
		SET started_at = now() - interval '2 hours' WHERE status = 'running'`)
	fresh, err := s.Start(ctx, spec1("other"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reap(ctx, "orders", time.Hour); err != nil || n != 1 {
		t.Fatalf("reap = %d %v, want the stale run", n, err)
	}
	var status, msg string
	if err := pool.QueryRow(ctx, `SELECT status, coalesce(error, '') FROM sage.review_run
		WHERE id = $1`, stale).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || msg != "sidecar stopped during run" {
		t.Fatalf("reaped run = %s %q", status, msg)
	}
	if n, _ := s.Reap(ctx, "other", time.Hour); n != 0 {
		t.Fatal("a fresh run of another database was reaped")
	}
	if err := s.Finish(ctx, fresh, RunDone, &Grade{RubricVersion: Version}, ""); err != nil {
		t.Fatalf("finish the fresh run: %v", err)
	}
	if _, err := s.Start(ctx, spec1("orders")); err != nil {
		t.Fatalf("a new run after the reap: %v", err)
	}
}

func TestStoreDB_InsertErrorNamesTheTable(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	pool.Close()
	_, err := s.Start(ctx, spec1("orders"))
	if err == nil || !strings.Contains(err.Error(), "sage.review_run") {
		t.Fatalf("start on a closed pool = %v, want an error naming sage.review_run", err)
	}
	if _, err := NewStore(nil).Start(ctx, spec1("orders")); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil pool = %v, want ErrNoStore", err)
	}
	if _, err := NewStore(nil).Latest(ctx, "orders", 2); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil pool latest = %v, want ErrNoStore", err)
	}
}

func TestStoreDB_RefusesInvalidRunSpecs(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	for _, bad := range []RunSpec{{Kind: KindGrade, RubricVersion: Version},
		{Database: "x", Kind: "audit", RubricVersion: Version},
		{Database: "x", Kind: KindGrade}} {
		if _, err := s.Start(ctx, bad); !errors.Is(err, ErrInvalidRun) {
			t.Errorf("Start(%+v) = %v, want ErrInvalidRun", bad, err)
		}
	}
}

type brokenFirstLook struct{}

func (brokenFirstLook) Latest(context.Context, string) (firstlook.Report, bool, error) {
	return firstlook.Report{}, false, errors.New("read sage.first_look: connection reset")
}

func TestRunnerDB_FirstLookErrorGivesAPartialRun(t *testing.T) {
	pool, ctx := livePool(t)
	loader := &DBLoader{Pool: pool, Database: "orders", FirstLook: brokenFirstLook{}}
	run := &Runner{Store: NewStore(pool), Loader: loader, Rubric: V1(), Database: "orders"}
	out, err := run.Run(ctx, KindGrade, []string{"test"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != RunPartial || out.LoadErr == nil ||
		!strings.Contains(out.LoadErr.Error(), "connection reset") {
		t.Fatalf("outcome = %s %v, want partial with the firstlook error", out.Status,
			out.LoadErr)
	}
	var status, msg string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM sage.review_run WHERE id = $1`,
		out.RunID).Scan(&status, &msg); err != nil || status != "partial" ||
		!strings.Contains(msg, "firstlook") {
		t.Fatalf("stored run = %s %q %v, want partial naming firstlook", status, msg, err)
	}
	dup := dimOf(t, out.Grade, "schema").Checks
	for _, c := range dup {
		if c.Check == "schema.duplicate_index" && (c.Status != StatusUnknown ||
			!strings.Contains(c.Reason, "firstlook")) {
			t.Fatalf("duplicate index = %+v, want unknown naming firstlook", c)
		}
	}
	if !out.Delta.FirstRun {
		t.Fatalf("first delta = %+v, want first run", out.Delta)
	}
}

func TestRunnerDB_ARunningRowMakesTheNextRunErrRunning(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	if _, err := s.Start(ctx, spec1("orders")); err != nil {
		t.Fatal(err)
	}
	run := &Runner{Store: s, Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: V1(), Database: "orders"}
	if _, err := run.Run(ctx, KindGrade, nil); !errors.Is(err, ErrRunning) {
		t.Fatalf("second run = %v, want ErrRunning", err)
	}
	other := &Runner{Store: s, Loader: &DBLoader{Pool: pool, Database: "billing"},
		Rubric: V1(), Database: "billing"}
	if out, err := other.Run(ctx, KindGrade, nil); err != nil || out.RunID == 0 {
		t.Fatalf("another database = %+v %v, want its own run", out, err)
	}
}

func TestRunnerDB_SecondRunDeltaComparesTheFirst(t *testing.T) {
	pool, ctx := livePool(t)
	run := &Runner{Store: NewStore(pool), Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: V1(), Database: "orders"}
	first, err := run.Run(ctx, KindGrade, []string{"first_look"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.Run(ctx, KindGrade, []string{"daily"})
	if err != nil {
		t.Fatal(err)
	}
	if second.RunID == first.RunID || second.Delta.FirstRun || !second.Delta.Empty() {
		t.Fatalf("second delta = %+v, want empty (nothing changed)", second.Delta)
	}
	runs, err := NewStore(pool).Latest(ctx, "orders", 2)
	if err != nil || len(runs) != 2 || runs[0].ID != second.RunID ||
		runs[1].ID != first.RunID || runs[0].Triggers[0] != "daily" {
		t.Fatalf("latest = %+v %v, want the two runs newest first", runs, err)
	}
}
