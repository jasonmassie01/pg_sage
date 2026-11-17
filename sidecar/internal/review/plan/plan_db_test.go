package plan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// review-03b on a real server: a built review plan is stored as one
// plan-01 plan with per-step origins and advice rows, a re-run finds it,
// a model step waits for its own approval until its model is granted
// authority, and parallel reviews keep their steps apart.

// seedCalls gives the fixture's statements enough calls for a verdict.
func seedCalls(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	hour := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	for _, qid := range []int64{11, 12, 14, 21, 31} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour,
			calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, epoch_break, pairs)
			VALUES ($1, $2, 500, 1, 1, 1, 1, 0, 0, false, 2)
			ON CONFLICT DO NOTHING`, qid, hour); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
}

// built is baseInput for a fresh run of db, built, with its pack stored.
func built(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string) Result {
	t.Helper()
	in := baseInput(t, db)
	in.RunID = newRun(t, ctx, pool, db)
	savePack(t, ctx, pool, in.Pack)
	res, err := Build(ctx, in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return res
}

func originsOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	planID int64) map[int]string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT s.ordinal, o.origin || ':' || o.origin_ref
		FROM sage.plan_step s JOIN sage.plan_step_origin o ON o.step_id = s.id
		WHERE s.plan_id = $1`, planID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var n int
		var o string
		if err := rows.Scan(&n, &o); err != nil {
			t.Fatal(err)
		}
		out[n] = o
	}
	return out
}

func TestSaveDB_StoresThePlanOriginsAndAdvice(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	res := built(t, ctx, pool, "shop")
	res.Advice = append(res.Advice, Advice{Origin: OriginModel, OriginRef: testModel,
		Kind: "drop_unused_index", Target: "public.orders_pkey_dup",
		Reason: ReasonNoTemplate, Detail: "no template for kind drop_unused_index"})
	saved, err := newStore(pool).Save(ctx, res)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.PlanID == 0 || saved.Reused || saved.GeneratorSteps != 1 || saved.ModelSteps != 1 ||
		saved.Advice != 1 || len(saved.PlanHash) != 64 {
		t.Fatalf("saved %+v", saved)
	}
	got := originsOf(t, ctx, pool, saved.PlanID)
	if got[1] != "generator:indexes" || got[2] != "model:"+testModel || len(got) != 2 {
		t.Fatalf("step origins %v", got)
	}
	var reason, detail string
	if err := pool.QueryRow(ctx, `SELECT reason, detail FROM sage.review_advice
		WHERE run_id = $1`, res.RunID).Scan(&reason, &detail); err != nil ||
		reason != ReasonNoTemplate || !strings.Contains(detail, "drop_unused_index") {
		t.Fatalf("advice row %q %q %v", reason, detail, err)
	}
	var measurement string
	if err := pool.QueryRow(ctx, `SELECT o.measurement::text FROM sage.plan_step_origin o
		JOIN sage.plan_step s ON s.id = o.step_id WHERE s.plan_id = $1 AND s.ordinal = 2`,
		saved.PlanID).Scan(&measurement); err != nil ||
		!strings.Contains(measurement, `"metric": "time_per_call"`) {
		t.Fatalf("measurement %s %v", measurement, err)
	}
}

func TestSaveDB_ModelStepWaitsForItsOwnApprovalUntilGranted(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	saved, err := newStore(pool).Save(ctx, built(t, ctx, pool, "shop"))
	if err != nil {
		t.Fatal(err)
	}
	plans := changeplan.NewStore(pool)
	if _, err := plans.Approve(ctx, saved.PlanID, saved.PlanHash, 7); err != nil {
		t.Fatalf("approve all: %v", err)
	}
	cov := coverage(t, ctx, plans, saved.PlanID)
	if !cov[0].Covered || cov[1].Covered ||
		!strings.HasPrefix(cov[1].Reason, changeplan.ReasonModelOrigin) {
		t.Fatalf("before G-M1: %+v, want the model step held", cov)
	}
	if err := GrantModelAuthority(ctx, pool, "M1 ", "admin:7", "G-M1 approved"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if ok, err := ModelAuthority(ctx, pool, testModel); err != nil || !ok {
		t.Fatalf("authority after the grant: %v %v", ok, err)
	}
	if cov = coverage(t, ctx, plans, saved.PlanID); !cov[1].Covered {
		t.Fatalf("after G-M1: %+v, want the model step covered by approve all", cov[1])
	}
	if err := RevokeModelAuthority(ctx, pool, testModel, "window failed"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if cov = coverage(t, ctx, plans, saved.PlanID); cov[1].Covered {
		t.Fatalf("after the revocation: %+v, want the model step held again", cov[1])
	}
	steps := planSteps(t, ctx, plans, saved.PlanID)
	if _, err := plans.ApproveStep(ctx, saved.PlanID, steps[1].ID, saved.PlanHash, 7,
		changeplan.ViaUI); err != nil {
		t.Fatalf("approve the model step: %v", err)
	}
	if cov = coverage(t, ctx, plans, saved.PlanID); !cov[1].Covered ||
		cov[1].By != changeplan.CoveredByStep {
		t.Fatalf("with its own approval: %+v, want covered by the step", cov[1])
	}
	if err := GrantModelAuthority(ctx, pool, testModel, " ", "x"); err == nil {
		t.Fatal("a grant with no grantor was accepted")
	}
}

func coverage(t *testing.T, ctx context.Context, plans *changeplan.Store,
	id int64) []changeplan.StepCoverage {
	t.Helper()
	d, err := plans.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	return d.Coverage
}

func planSteps(t *testing.T, ctx context.Context, plans *changeplan.Store,
	id int64) []changeplan.Step {
	t.Helper()
	p, err := plans.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Steps
}

func TestSaveDB_RerunOfOneReviewIsIdempotent(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	res := built(t, ctx, pool, "shop")
	store := newStore(pool)
	var wg sync.WaitGroup
	saves := make([]Saved, 3)
	errs := make([]error, 3)
	for i := range saves {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			saves[i], errs[i] = store.Save(ctx, res)
		}(i)
	}
	wg.Wait()
	reused := 0
	for i := range saves {
		if errs[i] != nil {
			t.Fatalf("save %d: %v", i, errs[i])
		}
		if saves[i].PlanID != saves[0].PlanID || saves[i].PlanHash != saves[0].PlanHash {
			t.Fatalf("saves %+v, want one plan", saves)
		}
		if saves[i].Reused {
			reused++
		}
	}
	var plans int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan`).Scan(&plans); err != nil ||
		plans != 1 || reused != 2 {
		t.Fatalf("%d plans (%v), %d reused saves; want 1 plan and 2 reused", plans, err, reused)
	}
}

func TestSaveDB_ParallelReviewsGetTheirOwnPlans(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	results := []Result{built(t, ctx, pool, "shop"), built(t, ctx, pool, "ledger")}
	store := newStore(pool)
	saves := make([]Saved, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			saves[i], errs[i] = store.Save(ctx, results[i])
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || saves[0].PlanID == saves[1].PlanID {
		t.Fatalf("saves %+v errors %v", saves, errs)
	}
	var shared int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan_step_origin a
		JOIN sage.plan_step_origin b ON a.step_id = b.step_id AND a.plan_id <> b.plan_id`).
		Scan(&shared); err != nil || shared != 0 {
		t.Fatalf("%d shared step rows (%v)", shared, err)
	}
}

func TestSaveDB_NoChangesProposedIsRecordedWithoutAPlan(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "shop")
	saved, err := newStore(pool).Save(ctx, Result{RunID: run, Message: NoChangesProposed})
	if err != nil || saved.PlanID != 0 || saved.Message != NoChangesProposed {
		t.Fatalf("saved %+v %v", saved, err)
	}
	var msg string
	if err := pool.QueryRow(ctx, `SELECT message FROM sage.review_plan WHERE run_id = $1
		AND plan_id IS NULL`, run).Scan(&msg); err != nil || msg != NoChangesProposed {
		t.Fatalf("review plan row %q %v", msg, err)
	}
}

type failingResolver struct{ acceptAll }

func (failingResolver) EvidenceExists(context.Context, string, string) (bool, error) {
	return false, errors.New("connection reset by peer")
}

func TestSaveDB_PlanInsertFailureNamesSagePlan(t *testing.T) {
	pool, ctx := livePool(t)
	res := built(t, ctx, pool, "shop")
	store := NewStore(pool, changeplan.NewStore(pool), failingResolver{})
	_, err := store.Save(ctx, res)
	if err == nil || !strings.Contains(err.Error(), "sage.plan") ||
		!strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("save: %v, want an error naming sage.plan and the cause", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_plan`).Scan(&rows); err != nil ||
		rows != 0 {
		t.Fatalf("%d review plan rows after a failed save (%v)", rows, err)
	}
	if _, err := store.Save(ctx, Result{}); err == nil {
		t.Fatal("a result with no run id was saved")
	}
}

// A record that fails after the plan was created voids the plan, so no
// orphan waits for an approval, and the error names the failed table. The
// step's origin "agent" breaks sage.plan_step_origin's CHECK, a long
// error that the void's reason must still fit (plan-01's 200 runes).
func TestSaveDB_RecordFailureVoidsTheCreatedPlan(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	res := built(t, ctx, pool, "shop")
	res.Steps[0].Origin = "agent"
	_, err := newStore(pool).Save(ctx, res)
	if err == nil || !strings.Contains(err.Error(), "sage.plan_step_origin") {
		t.Fatalf("save: %v, want an error naming sage.plan_step_origin", err)
	}
	var status string
	var plans, rows int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(status) FROM sage.plan`).
		Scan(&plans, &status); err != nil || plans != 1 || status != "voided" {
		t.Fatalf("%d plans, status %q (%v): want the created plan voided", plans, status, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_plan`).Scan(&rows); err != nil ||
		rows != 0 {
		t.Fatalf("%d review plan rows after a failed record (%v)", rows, err)
	}
}
