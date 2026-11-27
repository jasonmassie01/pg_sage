package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/verify"
)

// seedPreFixTransientRow stores the watch that a transient read error closed
// before executor-02: open, unverifiable, observation_unavailable, due now,
// with its hard deadline ahead. Resuming it unswept replays its revert.
func seedPreFixTransientRow(t *testing.T, ctx context.Context, f createFixture) {
	t.Helper()
	pool := f.exec.pool
	seedQuerySamples(t, ctx, pool, f.qid, f.executedAt.Add(-2*time.Hour), 119, 10, 20)
	seedContinuation(t, ctx, pool, f.qid, f.executedAt, 119, 10, 8)
	state := verify.WatchState{
		ID: fmt.Sprintf("index-action-%d", f.id), ActionID: f.id, ExecutedAt: f.executedAt,
		Table: "public." + f.table, IndexName: f.index,
		Criterion: verify.Criterion{Kind: "per_query_latency", TargetIDs: []int64{f.qid},
			MinGainPct: 20, RegressPct: 15, WriteImpactPct: 20,
			Window: 2 * time.Hour, HardMax: 72 * time.Hour},
		Status: "pending", Window: 2 * time.Hour,
		NextEvaluationAt: f.executedAt.Add(2 * time.Hour),
	}
	if err := verify.NewPostgresStateStore(pool, 30).Create(ctx, state); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.verification SET verdict='unverifiable',
		reason='observation_unavailable', next_evaluation_at=now()-interval '1 minute'
		WHERE action_log_id=$1`, f.id); err != nil {
		t.Fatalf("simulate the pre-fix row: %v", err)
	}
}

// failSweepFor makes the startup sweep's UPDATE fail while it would reopen
// this action's row; Claim and Update of the watch itself still work.
func failSweepFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) func() {
	t.Helper()
	name := fmt.Sprintf("sage_test_fail_sweep_%d", id)
	for _, sql := range []string{
		`CREATE FUNCTION public.` + name + `() RETURNS trigger LANGUAGE plpgsql AS
			$$BEGIN RAISE EXCEPTION 'simulated sweep failure'; END$$`,
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON sage.verification FOR EACH ROW
			WHEN (NEW.action_log_id = %d AND NEW.reason LIKE 'observation_retry: reopened%%')
			EXECUTE FUNCTION public.%s()`, name, id, name),
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("install sweep failure: %v", err)
		}
	}
	drop := func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TRIGGER IF EXISTS "+name+" ON sage.verification")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS public."+name+"()")
	}
	t.Cleanup(drop)
	return drop
}

func indexPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string) int {
	t.Helper()
	var present int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index
		WHERE indexrelid=to_regclass($1)`, index).Scan(&present); err != nil {
		t.Fatalf("read pg_index: %v", err)
	}
	return present
}

// capturedLog is declared in record_failure_test.go.
func (c *capturedLog) contains(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range c.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// State transition: while the sweep fails, the cycle's resume is withheld
// (the pre-fix row would replay its revert); once the sweep succeeds the
// row is observed and the index kept.
func TestResumeIndexVerificationWaitsForTheSweep(t *testing.T) {
	f, ctx := newCreateFixture(t)
	pool := f.exec.pool
	logs := &capturedLog{}
	f.exec.logFn = logs.logf
	seedPreFixTransientRow(t, ctx, f)
	restore := failSweepFor(t, ctx, pool, f.id)

	f.exec.resumeIndexVerification(ctx)
	if present := indexPresent(t, ctx, pool, f.index); present != 1 {
		t.Fatalf("index present = %d after a failed sweep, want kept", present)
	}
	if v := verificationVerdict(t, pool, f.id); v != "unverifiable" {
		t.Fatalf("verdict = %q after a failed sweep, want the untouched row", v)
	}
	if f.exec.retriesReopened.Load() || !logs.contains("resume withheld") ||
		!logs.contains("simulated sweep failure") {
		t.Fatalf("failed sweep: reopened=%v logs=%q", f.exec.retriesReopened.Load(), logs.lines)
	}

	restore()
	f.exec.resumeIndexVerification(ctx)
	if present := indexPresent(t, ctx, pool, f.index); present != 1 {
		t.Fatalf("index present = %d after the sweep, want kept", present)
	}
	if v := verificationVerdict(t, pool, f.id); v != "success" {
		t.Fatalf("verdict = %q after the sweep, want success (observed again)", v)
	}
	if !f.exec.retriesReopened.Load() {
		t.Fatal("a successful sweep must not run again")
	}
}

// The RunCycle path itself sweeps before the first resume.
func TestRunCycleSweepsBeforeFirstResume(t *testing.T) {
	f, ctx := newCreateFixture(t)
	seedPreFixTransientRow(t, ctx, f)
	f.exec.RunCycle(ctx, true)
	if present := indexPresent(t, ctx, f.exec.pool, f.index); present != 1 {
		t.Fatalf("index present = %d, want kept", present)
	}
	if v := verificationVerdict(t, f.exec.pool, f.id); v != "success" {
		t.Fatalf("verdict = %q, want success (observed again)", v)
	}
}

type resultEngine struct {
	results []verify.ResumeResult
	err     error
}

func (r *resultEngine) OKToApplyNow(context.Context) (verify.Admission, error) {
	return verify.Admission{}, errors.New("not used")
}

func (r *resultEngine) Watch(context.Context, verify.WatchRequest) (verify.Verdict, error) {
	return verify.Verdict{}, errors.New("not used")
}

func (r *resultEngine) ResumeDueResults(context.Context) ([]verify.ResumeResult, error) {
	return r.results, r.err
}

// Error propagation: one action's failed rollback-SQL lookup is reported
// with the engine's error, and the other actions' verdicts still resume.
func TestIndexVerifierResumeContinuesPastAFailedLookup(t *testing.T) {
	f, ctx := newCreateFixture(t)
	engineErr := errors.New("claim failed for another watch")
	missing := int64(-f.id)
	verifier := &postgresIndexVerifier{exec: f.exec, engine: &resultEngine{
		err: engineErr,
		results: []verify.ResumeResult{
			{ActionID: missing, Verdict: verify.Verdict{Revert: true, Status: "reverted"}},
			{ActionID: f.id, Verdict: verify.Verdict{Retain: true, Status: "success"}},
		},
	}}
	resumed, err := verifier.ResumeDue(ctx)
	if !errors.Is(err, engineErr) ||
		!strings.Contains(err.Error(), fmt.Sprintf("load rollback SQL for action %d", missing)) {
		t.Fatalf("ResumeDue error = %v, want the engine and the lookup error", err)
	}
	if len(resumed) != 1 || resumed[0].ActionID != f.id || !resumed[0].Verdict.Retain ||
		resumed[0].RollbackSQL != "DROP INDEX CONCURRENTLY IF EXISTS "+f.index {
		t.Fatalf("resumed = %#v, want only action %d with its rollback SQL", resumed, f.id)
	}
}
