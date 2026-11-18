package rubric

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// review-01a review fixes: a first look older than FirstLookMaxAge grades
// unknown, a run whose finish failed ends failed instead of running, and
// the migration safety detector counts as run only once a snapshot exists.
// No concurrent access tests here: the store's one-running-row rule is
// TestStoreDB_TwoRunsForOneDatabaseGiveOneRunningRow.

func TestEvaluate_StaleFirstLookIsUnknownAtTheAgeBoundary(t *testing.T) {
	c := CheckSpec{ID: "x.dup", Source: SourceFirstLook, Ref: firstlook.RuleRedundantIndex,
		Weight: 1}
	p := CheckSpec{ID: "x.ap08", Source: SourcePosture, Ref: "AP-08", Weight: 1}
	cases := []struct {
		age  time.Duration
		want Status
	}{
		{FirstLookMaxAge - time.Second, StatusPass},
		{FirstLookMaxAge, StatusPass},
		{FirstLookMaxAge + time.Second, StatusUnknown},
		{90 * 24 * time.Hour, StatusUnknown},
	}
	for _, tc := range cases {
		r := report()
		r.FinishedAt = evalNow.Add(-tc.age)
		in := Inputs{Now: evalNow, FirstLook: FirstLookInput{Found: true, Report: r}}
		for _, spec := range []CheckSpec{c, p} {
			got := one(t, spec, in)
			if got.Status != tc.want {
				t.Errorf("%s at age %s = %s %q, want %s", spec.Ref, tc.age, got.Status,
					got.Reason, tc.want)
			}
			if tc.want == StatusUnknown && (!strings.Contains(got.Reason, "firstlook") ||
				!strings.Contains(got.Reason, "7 days") ||
				!strings.Contains(got.Reason, "re-check")) {
				t.Errorf("%s stale reason = %q, want firstlook, 7 days and re-check",
					spec.Ref, got.Reason)
			}
		}
	}
}

// A stale first look only lowers a grade: the same report graded fresh
// and stale never scores higher stale.
func TestCompute_StaleFirstLookNeverRaisesTheGrade(t *testing.T) {
	r := report()
	r.FinishedAt = evalNow.Add(-time.Hour)
	fresh := Inputs{Now: evalNow, FirstLook: FirstLookInput{Found: true, Report: r}}
	stale := fresh
	stale.Now = evalNow.Add(FirstLookMaxAge + time.Hour)
	f, s := Compute(V1(), Evaluate(V1(), fresh)), Compute(V1(), Evaluate(V1(), stale))
	if msg := monotone(f, s); msg != "" {
		t.Fatalf("stale first look: %s", msg)
	}
	redundant := statusOf(t, s, "schema.redundant_index")
	if was := statusOf(t, f, "schema.redundant_index"); was.Status != StatusPass ||
		redundant.Status != StatusUnknown {
		t.Fatalf("redundant index fresh %s, stale %s: want pass then unknown", was.Status,
			redundant.Status)
	}
}

// A rubric with a repeated dimension id breaks sage.review_grade's key, so
// Finish fails; the run must end failed with that error, and the next run
// must not see it as running.
func TestRunnerDB_FinishErrorFailsTheRunInsteadOfLeavingItRunning(t *testing.T) {
	pool, ctx := livePool(t)
	spec := CheckSpec{ID: "x.a", Source: SourceSetting, Ref: refPendingRestart, Weight: 1}
	twin := spec
	twin.ID = "x.b"
	bad := Rubric{Version: Version, Dimensions: []Dimension{
		{ID: "x", Title: "X", Checks: []CheckSpec{spec}},
		{ID: "x", Title: "X", Checks: []CheckSpec{twin}}}}
	run := &Runner{Store: NewStore(pool), Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: bad, Database: "orders"}
	out, err := run.Run(ctx, KindGrade, []string{"test"})
	if err == nil || !strings.Contains(err.Error(), "sage.review_grade") {
		t.Fatalf("run = %v, want the review_grade insert error", err)
	}
	var status, msg string
	if err := pool.QueryRow(ctx, `SELECT status, coalesce(error, '') FROM sage.review_run
		WHERE id = $1`, out.RunID).Scan(&status, &msg); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != string(RunFailed) || !strings.Contains(msg, "review_grade") {
		t.Fatalf("stored run = %s %q, want failed naming review_grade", status, msg)
	}
	good := &Runner{Store: NewStore(pool), Loader: &DBLoader{Pool: pool, Database: "orders"},
		Rubric: V1(), Database: "orders"}
	if next, err := good.Run(ctx, KindGrade, []string{"test"}); err != nil ||
		next.Status != RunDone {
		t.Fatalf("next run = %+v %v, want done (no row left running)", next, err)
	}
}

func TestLoaderDB_MigrationSafetyNeedsASnapshot(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool, "DELETE FROM sage.findings WHERE category = 'migration_safety'",
		"DELETE FROM sage.snapshots")
	l := &DBLoader{Pool: pool, Database: "orders"}
	ran, err := l.producersRan(ctx)
	if err != nil {
		t.Fatalf("producers: %v", err)
	}
	if ran["migration"] || ran["analyzer"] {
		t.Fatalf("ran = %v, want neither the analyzer nor migration without a snapshot", ran)
	}
	in, _ := l.Load(ctx)
	got := one(t, CheckSpec{ID: "x.mig", Source: SourceFinding, Ref: "migration_safety",
		Weight: 2}, in)
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "migration") {
		t.Fatalf("migration safety = %+v, want unknown naming its producer", got)
	}
}
