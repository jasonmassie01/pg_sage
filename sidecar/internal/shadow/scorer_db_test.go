package shadow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration (real Postgres): the scorer reads, in the monitored
// database, what happened to each pending shadow decision and scores it
// by the best evidence available (operator > applied > what-if >
// unscored). A score is written once (status pending -> scored); two
// scorers racing over the same decision score it once.

func newScorer(pool *pgxpool.Pool, o Options) *Scorer {
	return NewScorer(pool, o, func(string, ...any) {})
}

func runOnce(t *testing.T, s *Scorer) Result {
	t.Helper()
	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	return res
}

// queueDecision records an operator's decision on a proposal of sql.
func queueDecision(t *testing.T, pool *pgxpool.Pool, sql, status string,
	actionLogID int64, decidedAgo time.Duration) int64 {
	t.Helper()
	var id int64
	var action any
	if actionLogID > 0 {
		action = actionLogID
	}
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, action_risk, status, decided_by, decided_at,
		 action_log_id, action_type)
		VALUES (77, $1, 'moderate', $2, 7, now() - make_interval(secs => $3), $4,
		        'create_index_concurrently') RETURNING id`,
		sql, status, decidedAgo.Seconds(), action).Scan(&id); err != nil {
		t.Fatalf("queue item: %v", err)
	}
	return id
}

// executedAction records an action that ran executedAgo with its
// lifecycle outcome and, unless empty, its decided verdict.
func executedAction(t *testing.T, pool *pgxpool.Pool, actionType, sql, lifecycle,
	verdict string, executedAgo time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		VALUES ($1, $2, $3, now() - make_interval(secs => $4)) RETURNING id`,
		actionType, sql, lifecycle, executedAgo.Seconds()).Scan(&id); err != nil {
		t.Fatalf("action_log: %v", err)
	}
	if verdict == "" {
		return id
	}
	decided := "now()"
	if verdict == "pending" {
		decided = "NULL"
	}
	mustExec(t, pool, `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method, verdict, tolerance,
		 decided_at) VALUES ($1, 'index_create', '{}', 'model', $2, 'unmeasured', `+
		decided+`)`, id, verdict)
	return id
}

func assertScore(t *testing.T, s *Store, id int64, score, source string, counted bool) Decision {
	t.Helper()
	d := get(t, s, id)
	if d.Status != StatusScored || d.Score != score || d.ScoreSource != source ||
		d.Counted != counted || d.ScoredAt == nil || d.ScoreReason == "" {
		t.Fatalf("decision %d: status=%s score=%s source=%s counted=%v reason=%q, "+
			"want scored %s/%s counted=%v", id, d.Status, d.Score, d.ScoreSource, d.Counted,
			d.ScoreReason, score, source, counted)
	}
	return d
}

func assertPending(t *testing.T, s *Store, id int64) {
	t.Helper()
	if d := get(t, s, id); d.Status != StatusPending || d.Score != "" {
		t.Fatalf("decision %d: %s/%s, want pending", id, d.Status, d.Score)
	}
}

func TestScorerOperatorRejectionIsIncorrect(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_rej ON public.o (rej)`
	d := record(t, s, sample("index_create", sql))
	age(t, pool, d.ID, 2*time.Hour)
	q := queueDecision(t, pool, sql, "rejected", 0, time.Hour)
	before := scoreCount("orders", "index_create", ScoreIncorrect, SourceOperator)
	res := runOnce(t, newScorer(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreIncorrect, SourceOperator, false)
	if got.RefQueueID != q || res.Scored[SourceOperator] != 1 || res.Examined != 1 {
		t.Fatalf("ref queue %d (want %d), result %+v", got.RefQueueID, q, res)
	}
	if scoreCount("orders", "index_create", ScoreIncorrect, SourceOperator)-before != 1 {
		t.Fatal("score metric not counted")
	}
	// Scoring again changes nothing.
	again := runOnce(t, newScorer(pool, fastOptions()))
	if again.Examined != 0 || get(t, s, d.ID).ScoredAt.IsZero() {
		t.Fatalf("second pass: %+v", again)
	}
}

func TestScorerApprovedAndImprovedIsCorrect(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_ok ON public.o (ok)`
	d := record(t, s, sample("index_create", sql))
	age(t, pool, d.ID, 3*time.Hour)
	act := executedAction(t, pool, "create_index", sql, "success", "improved", 2*time.Hour)
	queueDecision(t, pool, sql, "executed", act, 2*time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceOperator, false)
	if got.RefActionLogID != act {
		t.Fatalf("ref action %d, want %d", got.RefActionLogID, act)
	}
}

func TestScorerWaitsForTheVerdictOfAnApproval(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_wait ON public.o (w)`
	d := record(t, s, sample("index_create", sql))
	age(t, pool, d.ID, 26*time.Hour)
	act := executedAction(t, pool, "create_index", sql, "monitoring", "pending", time.Hour)
	queueDecision(t, pool, sql, "executed", act, time.Hour)
	res := runOnce(t, newScorer(pool, fastOptions()))
	assertPending(t, s, d.ID)
	if res.Waiting != 1 {
		t.Fatalf("result %+v, want one waiting", res)
	}
	mustExec(t, pool, `UPDATE sage.action_outcome SET verdict = 'regressed',
		decided_at = now() WHERE action_log_id = $1`, act)
	mustExec(t, pool, `UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id = $1`, act)
	runOnce(t, newScorer(pool, fastOptions()))
	assertScore(t, s, d.ID, ScoreIncorrect, SourceOperator, false)
}

func TestScorerIgnoresDecisionsMadeBeforeTheShadow(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `ALTER SYSTEM SET work_mem = '64MB'`
	d := record(t, s, sample("config_guc", sql))
	age(t, pool, d.ID, time.Hour)
	queueDecision(t, pool, sql, "rejected", 0, 2*time.Hour)
	executedAction(t, pool, "alter_system", sql, "success", "improved", 3*time.Hour)
	res := runOnce(t, newScorer(pool, fastOptions()))
	assertPending(t, s, d.ID)
	if res.Waiting != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestScorerAppliedThroughPgSageMatchesByShape(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_shadow_name ON public.o (shape_col)`))
	age(t, pool, d.ID, 5*time.Hour)
	// The operator ran the same change by hand, under another name.
	act := executedAction(t, pool, "create_index",
		`create index concurrently if not exists other_name on o ("shape_col")`,
		"success", "improved", time.Hour)
	// An unrelated action is not evidence.
	executedAction(t, pool, "create_index", `CREATE INDEX x ON public.o (other_col)`,
		"rolled_back", "regressed", time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceApplied, false)
	if got.RefActionLogID != act {
		t.Fatalf("ref action %d, want %d", got.RefActionLogID, act)
	}
}

func TestScorerOperatorOutranksAppliedEvidence(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_both ON public.o (both_col)`
	d := record(t, s, sample("index_create", sql))
	age(t, pool, d.ID, 5*time.Hour)
	executedAction(t, pool, "create_index", sql, "success", "improved", 3*time.Hour)
	q := queueDecision(t, pool, sql, "rejected", 0, 4*time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreIncorrect, SourceOperator, false)
	if got.RefQueueID != q {
		t.Fatalf("ref queue %d, want %d", got.RefQueueID, q)
	}
}

func TestScorerUnscoredAtTheHorizon(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	d := record(t, s, sample("config_guc", `ALTER SYSTEM SET work_mem = '32MB'`))
	young := record(t, s, sample("vacuum", `VACUUM public.o`))
	o := fastOptions()
	age(t, pool, d.ID, o.Horizon+time.Minute)
	age(t, pool, young.ID, o.Horizon-time.Hour)
	runOnce(t, newScorer(pool, o))
	assertScore(t, s, d.ID, ScoreUnscored, SourceNone, false)
	assertPending(t, s, young.ID)
}

func TestScorerEmptyLedgerAndNoPool(t *testing.T) {
	pool, ctx := testPool(t)
	res := runOnce(t, newScorer(pool, fastOptions()))
	if res.Examined != 0 || res.Waiting != 0 || len(res.Scored) != 0 {
		t.Fatalf("empty ledger: %+v", res)
	}
	if _, err := NewScorer(nil, fastOptions(), nil).RunOnce(ctx); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("nil pool: %v", err)
	}
}

func TestConcurrentScorersScoreOnce(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_race ON public.o (race)`
	d := record(t, s, sample("index_create", sql))
	age(t, pool, d.ID, 2*time.Hour)
	queueDecision(t, pool, sql, "rejected", 0, time.Hour)
	before := scoreCount("orders", "index_create", ScoreIncorrect, SourceOperator)
	var wg sync.WaitGroup
	scored := make([]int, 4)
	for i := range scored {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := newScorer(pool, fastOptions()).RunOnce(context.Background())
			if err == nil {
				scored[i] = res.Scored[SourceOperator]
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, n := range scored {
		total += n
	}
	if total != 1 {
		t.Fatalf("scored %d times across racing scorers, want 1 (%v)", total, scored)
	}
	if scoreCount("orders", "index_create", ScoreIncorrect, SourceOperator)-before != 1 {
		t.Fatal("racing scorers counted the score more than once")
	}
	assertScore(t, s, d.ID, ScoreIncorrect, SourceOperator, false)
}
