package modelgate

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/review/plan"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/modelgate"))
}

var tick0 = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", i+1, err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE sage.review_run, sage.plan, sage.review_gate,
		sage.review_model_authority CASCADE`); err != nil {
		t.Fatalf("empty the review tables: %v", err)
	}
	t.Cleanup(func() { assertNoGrantWithoutApproval(t, pool) })
	return pool, ctx
}

// assertNoGrantWithoutApproval is CHECK-39 over whatever path the test
// took: no granted row lacks its approval, and every live or revoked
// authority grant has a G-M1 row approved by the same person.
func assertNoGrantWithoutApproval(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var bad, orphan int
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sage.review_gate WHERE verdict = 'granted'
		   AND (approved_by IS NULL OR approved_at IS NULL)),
		(SELECT count(*) FROM sage.review_model_authority a WHERE NOT EXISTS (
		   SELECT 1 FROM sage.review_gate g WHERE g.model = a.model AND g.rule = 'G-M1'
		     AND g.approved_by = a.granted_by))`).Scan(&bad, &orphan)
	if err != nil || bad != 0 || orphan != 0 {
		t.Errorf("CHECK-39: %d grants without approval, %d authority rows without an "+
			"approved G-M1 row (%v)", bad, orphan, err)
	}
}

// seedReviews writes n done reviews of shop on model, each with one
// generator step (improved for the first genImproved, else no change) and
// one model step (improved for the first modelImproved, reverted for the
// first modelReverted of the rest, else no change). modelOnly says whether
// the model step is accepted; a not accepted model step stays pending.
type mix struct {
	n, genImproved, modelImproved, modelReverted int
	modelOnly                                    bool
	model                                        string
}

func seedReviews(t *testing.T, ctx context.Context, pool *pgxpool.Pool, m mix) {
	t.Helper()
	reverted := 0
	for i := 0; i < m.n; i++ {
		gen := reviewtest.Outcome{Origin: "generator", Ref: "indexes", Verdict: "no_change",
			Accepted: true}
		if i < m.genImproved {
			gen.Verdict = "improved"
		}
		mod := reviewtest.Outcome{Origin: "model", Ref: m.model, Verdict: "no_change",
			Accepted: true}
		switch {
		case i < m.modelImproved:
			mod.Verdict = "improved"
		case reverted < m.modelReverted:
			mod.Verdict, mod.Reverted = "regressed", true
			reverted++
		}
		if !m.modelOnly {
			mod = reviewtest.Outcome{Origin: "model", Ref: m.model}
		}
		_, err := reviewtest.SeedReview(ctx, pool, reviewtest.Review{Database: "shop",
			Models: []string{m.model}, Steps: []reviewtest.Outcome{gen, mod}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newGate(pool *pgxpool.Pool) *Gate {
	return New(pool, "shop", Options{Thresholds: DefaultThresholds()})
}

func evaluate(t *testing.T, ctx context.Context, g *Gate, model string, tick time.Time) Result {
	t.Helper()
	res, err := g.Evaluate(ctx, model, tick)
	if err != nil {
		t.Fatalf("Evaluate %s at %s: %v", model, tick, err)
	}
	return res
}

func authority(t *testing.T, ctx context.Context, pool *pgxpool.Pool, model string) bool {
	t.Helper()
	live, err := plan.ModelAuthority(ctx, pool, model)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

// Happy path, then the whole G-M1 path: eligible files one promotion; a
// person's approval grants the flag; a seeded regression revokes it.
func TestGateDB_PassFilesOnePromotionApprovalGrantsRegressionRevokes(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 40,
		modelOnly: true, model: "m1"})
	g := newGate(pool)
	res := evaluate(t, ctx, g, "OpenAI/M1", tick0)
	if res.Model != "m1" || !res.M1.Eligible || res.M1State != StatePending ||
		!strings.Contains(res.M1.Reason, "50 of 50 reviews") {
		t.Fatalf("result = %+v, want m1 pending approval with the numbers", res)
	}
	pending, err := g.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].Model != "m1" ||
		pending[0].Verdict != StatePending || pending[0].Evidence.Reviews.K != 50 {
		t.Fatalf("pending = %+v %v, want one promotion item with the evidence", pending, err)
	}
	if authority(t, ctx, pool, "m1") {
		t.Fatal("a pass granted authority before any approval")
	}
	got, err := g.Approve(ctx, pending[0].ID, "admin@example.com")
	if err != nil || got.Verdict != StateGranted || got.ApprovedBy != "admin@example.com" {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if !authority(t, ctx, pool, "m1") {
		t.Fatal("the approval did not set review-03b's authority flag")
	}
	if res := evaluate(t, ctx, g, "m1", tick0.Add(time.Hour)); res.M1State != StateGranted {
		t.Fatalf("a passing window after the grant: %+v, want still granted", res)
	}
	if p, _ := g.Pending(ctx); len(p) != 0 {
		t.Fatalf("pending after the grant = %+v, want none", p)
	}
	seedReviews(t, ctx, pool, mix{n: 2, genImproved: 2, modelReverted: 2, modelOnly: true,
		model: "m1"})
	res = evaluate(t, ctx, g, "m1", tick0.Add(2*time.Hour))
	if res.M1State != StateRevoked || !strings.Contains(res.M1.Reason, "reverts 2/50") {
		t.Fatalf("after a seeded regression: %+v, want revoked on reverts", res)
	}
	if authority(t, ctx, pool, "m1") {
		t.Fatal("the failed window left the authority flag live")
	}
	notes, err := g.Notes(ctx, tick0.Add(-24*time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(notes) != 2 || !strings.Contains(notes[0], "granted") ||
		!strings.Contains(notes[1], "revoked") {
		t.Fatalf("digest notes = %q %v, want the grant then the revocation", notes, err)
	}
}

// A grant for model A does not apply to model B, and a model change
// revokes A's grant at once.
func TestGateDB_ModelChangeRevokesAndDoesNotCarryOver(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 45,
		modelOnly: true, model: "m1"})
	g := newGate(pool)
	evaluate(t, ctx, g, "m1", tick0)
	pending, err := g.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	if _, err := g.Approve(ctx, pending[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	res := evaluate(t, ctx, g, "m2", tick0.Add(time.Hour))
	if len(res.Revoked) != 1 || res.Revoked[0] != "m1" || res.M1State != StateNotEligible ||
		!strings.Contains(res.M1.Reason, "0 of 50 reviews") {
		t.Fatalf("model change: %+v, want m1 revoked and m2 not eligible", res)
	}
	if authority(t, ctx, pool, "m1") || authority(t, ctx, pool, "m2") {
		t.Fatal("authority survived the model change or carried over to m2")
	}
	if res := evaluate(t, ctx, g, "", tick0.Add(2*time.Hour)); res.M1State != "" ||
		len(res.Revoked) != 0 {
		t.Fatalf("no running model: %+v, want nothing evaluated", res)
	}
}

func TestGateDB_ApproveRefusesWhatIsNotTheLivePendingItem(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 45,
		modelOnly: true, model: "m1"})
	g := newGate(pool)
	evaluate(t, ctx, g, "m1", tick0)
	first, _ := g.Pending(ctx)
	evaluate(t, ctx, g, "m1", tick0.Add(time.Hour))
	second, _ := g.Pending(ctx)
	if len(first) != 1 || len(second) != 1 || first[0].ID == second[0].ID {
		t.Fatalf("pending items %+v then %+v, want a newer one", first, second)
	}
	if _, err := g.Approve(ctx, first[0].ID, "admin"); !errors.Is(err, ErrStale) {
		t.Fatalf("approving a superseded item: %v, want ErrStale", err)
	}
	if _, err := g.Approve(ctx, second[0].ID, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("approving with no approver: %v, want ErrInvalidInput", err)
	}
	if _, err := g.Approve(ctx, 999999, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approving an unknown id: %v, want ErrNotFound", err)
	}
	m2 := latestRow(t, ctx, pool, RuleM2)
	if _, err := g.Approve(ctx, m2, "admin"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving a G-M2 row: %v, want ErrNotPending", err)
	}
	if _, err := g.Approve(ctx, second[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Approve(ctx, second[0].ID, "admin"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving twice: %v, want ErrNotPending", err)
	}
	if authority(t, ctx, pool, "m1") != true {
		t.Fatal("the live approval did not grant")
	}
}

func latestRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rule string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT max(id) FROM sage.review_gate WHERE rule = $1`,
		rule).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Two evaluations at once write one row per model, rule and tick.
func TestGateDB_ConcurrentEvaluationsWriteOneRowPerTick(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 45,
		modelOnly: true, model: "m1"})
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = newGate(pool).Evaluate(ctx, "m1", tick0)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("evaluation %d: %v", i, err)
		}
	}
	var m1, m2 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE rule = 'G-M1'),
		count(*) FILTER (WHERE rule = 'G-M2') FROM sage.review_gate
		WHERE model = 'm1' AND tick = $1`, tick0).Scan(&m1, &m2); err != nil {
		t.Fatal(err)
	}
	if m1 != 1 || m2 != 1 {
		t.Fatalf("rows for one tick: G-M1 %d, G-M2 %d, want one each", m1, m2)
	}
	if p, _ := newGate(pool).Pending(ctx); len(p) != 1 {
		t.Fatalf("pending = %+v, want one promotion item", p)
	}
}

// G-M2: no accepted model-only step over 50 reviews stops the model pass
// for that model; an operator's on restores it; a new model id starts fresh.
func TestGateDB_GM2DropsTheModelPassPerModel(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, model: "m1"})
	g := newGate(pool)
	res := evaluate(t, ctx, g, "m1", tick0)
	if res.M2State != StateModelOff || !res.M2.Eligible {
		t.Fatalf("no model-only steps: %+v, want model_off", res)
	}
	if res.M1.Eligible || !strings.Contains(res.M1.Reason, "no decided model steps") {
		t.Fatalf("G-M1 with no decided model steps: %+v", res.M1)
	}
	off, why, err := g.ModelPassDropped(ctx, "M1")
	if err != nil || !off || !strings.Contains(why, "G-M2") {
		t.Fatalf("dropped = %v %q %v, want the pass dropped for m1", off, why, err)
	}
	if off, _, err := g.ModelPassDropped(ctx, "m2"); err != nil || off {
		t.Fatalf("m2 = %v %v, want a fresh record (model on)", off, err)
	}
	on := New(pool, "shop", Options{Thresholds: DefaultThresholds(),
		OperatorOn: func() bool { return true }})
	if res := evaluate(t, ctx, on, "m1", tick0.Add(time.Hour)); res.M2State != StateModelOn {
		t.Fatalf("operator on: %+v, want model_on", res)
	}
	if off, _, _ := g.ModelPassDropped(ctx, "m1"); off {
		t.Fatal("the operator's on did not restore the model pass")
	}
	notes, err := g.Notes(ctx, tick0.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(notes) != 2 || !strings.Contains(notes[0], "stopped") ||
		!strings.Contains(notes[1], "again") {
		t.Fatalf("notes = %q %v, want the drop and the restore", notes, err)
	}
}

func TestGateDB_ErrorsAreNeverEligible(t *testing.T) {
	if _, err := New(nil, "shop", Options{}).Evaluate(context.Background(), "m1",
		tick0); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil pool: %v, want ErrNoStore", err)
	}
	var nilGate *Gate
	if _, err := nilGate.Evaluate(context.Background(), "m1", tick0); !errors.Is(err,
		ErrNoStore) {
		t.Fatalf("nil gate: %v, want ErrNoStore", err)
	}
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 45,
		modelOnly: true, model: "m1"})
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.plan_step_origin RENAME TO pso_gone`); err !=
		nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE sage.pso_gone
			RENAME TO plan_step_origin`)
	})
	_, err := newGate(pool).Evaluate(ctx, "m1", tick0)
	if err == nil || !strings.Contains(err.Error(), "review steps") {
		t.Fatalf("missing table: %v, want the scorecard query named", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_gate`).Scan(&rows); err !=
		nil || rows != 0 {
		t.Fatalf("%d gate rows after a failed read (%v), want none", rows, err)
	}
	if bad := New(pool, "shop", Options{Thresholds: Thresholds{MinReviews: -1}}); bad != nil {
		if _, err := bad.Evaluate(ctx, "m1", tick0); err == nil {
			t.Fatal("a gate with invalid thresholds evaluated")
		}
	}
}
