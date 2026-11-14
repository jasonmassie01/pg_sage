package tuner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// Revalidation against real pg_stat_statements (tuning-01): summed calls
// across roles, a pg_stat_statements_reset() that must not break hints, a
// fast hinted query that keeps its hint over several passes, a missing
// pg_stat_statements_info that fails closed, and Revalidate alongside Tune.

const (
	revalRolesCols  = 71
	revalResetCols  = 73
	revalPassesCols = 79
)

// seedProbeHint records an active hint for a real queryid (outside the
// reserved reval range) and deletes it when the test ends.
func seedProbeHint(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	qid int64, hintText string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO sage.query_hints (queryid, hint_text, symptom, status)
VALUES ($1, $2, 'disk_sort', 'active') RETURNING id`, qid, hintText).Scan(&id); err != nil {
		t.Fatalf("seed probe hint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.query_hints WHERE queryid = $1", qid)
	})
	return id
}

type hintState struct {
	status     string
	checkpoint *int64
	resetAt    *time.Time
	reverify   *time.Time
}

func readHintState(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id int64) hintState {
	t.Helper()
	var s hintState
	if err := pool.QueryRow(ctx, `
SELECT status, calls_at_last_check, pgss_reset_at, reverify_due
FROM sage.query_hints WHERE id = $1`, id).Scan(
		&s.status, &s.checkpoint, &s.resetAt, &s.reverify); err != nil {
		t.Fatalf("read hint %d: %v", id, err)
	}
	return s
}

func hintReverifyDue(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id int64) bool {
	t.Helper()
	return readHintState(t, ctx, pool, id).reverify != nil
}

func countBroken(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.query_hints WHERE status = 'broken'").Scan(&n); err != nil {
		t.Fatalf("count broken hints: %v", err)
	}
	return n
}

func runProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cols, runs int) {
	t.Helper()
	probeSQL, _ := buildUniqueProbe(cols)
	for i := 0; i < runs; i++ {
		if _, err := pool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
}

func mustRevalidate(t *testing.T, ctx context.Context, tu *Tuner) RevalidateReport {
	t.Helper()
	rpt, err := tu.Revalidate(ctx)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	return rpt
}

// ensureProbeRoles creates two NOLOGIN roles that run the probe through
// SET ROLE, so pg_stat_statements keeps one row per role.
func ensureProbeRoles(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	roles := []string{"sage_reval_role_a", "sage_reval_role_b"}
	for _, r := range roles {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN
    CREATE ROLE %s NOLOGIN;
  END IF; END $$`, r, r)); err != nil {
			t.Fatalf("create role %s: %v", r, err)
		}
		if _, err := pool.Exec(ctx, "GRANT "+r+" TO current_user"); err != nil {
			t.Fatalf("grant role %s: %v", r, err)
		}
	}
	t.Cleanup(func() {
		for _, r := range roles {
			_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+r)
		}
	})
	return roles
}

// runProbeAs runs the probe runs times under role on one connection.
func runProbeAs(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	role string, cols, runs int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatalf("set role %s: %v", role, err)
	}
	probeSQL, _ := buildUniqueProbe(cols)
	for i := 0; i < runs; i++ {
		if _, err := conn.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe as %s: %v", role, err)
		}
	}
}

type pgssRow struct {
	qid   int64
	calls int64
	total float64
}

// probeRows reads every pg_stat_statements row of the probe in this
// database (one per role).
func probeRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cols int) []pgssRow {
	t.Helper()
	_, normalized := buildUniqueProbe(cols)
	rows, err := pool.Query(ctx, `
SELECT queryid, calls, total_exec_time FROM pg_stat_statements
WHERE query = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		normalized)
	if err != nil {
		t.Fatalf("read probe rows: %v", err)
	}
	defer rows.Close()
	var out []pgssRow
	for rows.Next() {
		var r pgssRow
		if err := rows.Scan(&r.qid, &r.calls, &r.total); err != nil {
			t.Fatalf("scan probe row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// Two roles run the same statement: pg_stat_statements keeps a row per
// role, and fetchQueryStats sums them (the old LIMIT 1 read one of them).
func TestFetchQueryStats_SumsAcrossRoles(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	roles := ensureProbeRoles(t, ctx, pool)

	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		runProbeAs(t, ctx, pool, roles[0], revalRolesCols, 3)
		runProbeAs(t, ctx, pool, roles[1], revalRolesCols, 4)
		rows := probeRows(t, ctx, pool, revalRolesCols)
		if len(rows) < 2 {
			return []string{fmt.Sprintf("probe rows = %d, want one per role", len(rows))}
		}
		var calls int64
		var total float64
		for _, r := range rows {
			calls += r.calls
			total += r.total
		}
		obs, err := tu.fetchQueryStats(ctx, rows[0].qid)
		if err != nil {
			t.Fatalf("fetchQueryStats: %v", err)
		}
		var problems []string
		if !obs.Found || obs.Calls != calls || calls < 7 {
			problems = append(problems, fmt.Sprintf(
				"calls = %d found=%v, want the sum over %d rows = %d (>= 7)",
				obs.Calls, obs.Found, len(rows), calls))
		}
		if want := total / float64(calls); math.Abs(obs.MeanMs-want) > 1e-6 {
			problems = append(problems, fmt.Sprintf(
				"mean = %f, want sum(total)/sum(calls) = %f", obs.MeanMs, want))
		}
		return problems
	})
}

// CHECK-01: pg_stat_statements_reset() re-baselines the checkpoint and
// leaves the hint active; nothing new is marked broken.
func TestRevalidate_StatsResetKeepsHintActive(t *testing.T) {
	// Only a reset of every database moves pg_stat_statements_info's
	// stats_reset, which this test re-baselines on: exclusive server.
	pool, ctx := exclusiveTunerDB(t, "the pg_stat_statements stats_reset epoch")
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	runProbe(t, ctx, pool, revalResetCols, 3)
	rows := probeRows(t, ctx, pool, revalResetCols)
	if len(rows) != 1 {
		t.Fatalf("probe rows = %d, want 1", len(rows))
	}
	id := seedProbeHint(t, ctx, pool, rows[0].qid, `Set(work_mem "64MB")`)

	mustRevalidate(t, ctx, tu)
	first := readHintState(t, ctx, pool, id)
	if first.status != "active" || first.checkpoint == nil || *first.checkpoint < 3 ||
		first.resetAt == nil {
		t.Fatalf("first pass = %+v, want active with checkpoint >= 3 and stats_reset", first)
	}
	broken := countBroken(t, ctx, pool)

	if _, err := pool.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		t.Fatalf("pg_stat_statements_reset: %v", err)
	}
	runProbe(t, ctx, pool, revalResetCols, 1)
	rpt := mustRevalidate(t, ctx, tu)
	second := readHintState(t, ctx, pool, id)
	if second.status != "active" {
		t.Fatalf("status after reset = %q, want active", second.status)
	}
	if second.checkpoint == nil || *second.checkpoint != 1 {
		t.Fatalf("checkpoint after reset = %v, want re-baselined to 1", second.checkpoint)
	}
	if second.resetAt == nil || !second.resetAt.After(*first.resetAt) {
		t.Fatalf("pgss_reset_at = %v, want later than %v", second.resetAt, first.resetAt)
	}
	if rpt.Reset < 1 {
		t.Fatalf("report = %+v, want at least one stats reset", rpt)
	}
	if got := countBroken(t, ctx, pool); got != broken {
		t.Fatalf("broken hints = %d after the reset, want unchanged %d", got, broken)
	}

	runProbe(t, ctx, pool, revalResetCols, 2)
	mustRevalidate(t, ctx, tu)
	third := readHintState(t, ctx, pool, id)
	if third.status != "active" || third.checkpoint == nil || *third.checkpoint != 3 {
		t.Fatalf("third pass = %+v, want active with checkpoint 3", third)
	}
}

// CHECK-02: a hinted query under 100 ms stays active over three passes,
// its hint_plan.hints row stays, and no removal finding is emitted. A pass
// with no new calls makes it dormant, still installed and still covered
// against a new proposal; the next call makes it active again.
func TestRevalidate_FastHintSurvivesPasses(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	runProbe(t, ctx, pool, revalPassesCols, 2)
	rows := probeRows(t, ctx, pool, revalPassesCols)
	if len(rows) != 1 {
		t.Fatalf("probe rows = %d, want 1", len(rows))
	}
	qid, hint := rows[0].qid, `Set(work_mem "64MB")`
	id := seedProbeHint(t, ctx, pool, qid, hint)
	if _, err := pool.Exec(ctx, BuildInsertSQL(qid, hint)); err != nil {
		t.Fatalf("install hint: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), BuildDeleteSQL(qid, hint)) })

	for pass := 1; pass <= 3; pass++ {
		runProbe(t, ctx, pool, revalPassesCols, 2)
		mustRevalidate(t, ctx, tu)
		if s := readHintState(t, ctx, pool, id); s.status != "active" {
			t.Fatalf("pass %d: status = %q, want active", pass, s.status)
		}
	}
	assertInstalledAndKept(t, ctx, tu, qid)

	mustRevalidate(t, ctx, tu) // no calls since the last pass
	if s := readHintState(t, ctx, pool, id); s.status != "dormant" {
		t.Fatalf("quiet pass: status = %q, want dormant", s.status)
	}
	assertInstalledAndKept(t, ctx, tu, qid)
	// tuning-02: covered means refused by the durable check (the map
	// bootstrap it replaced is gone).
	if err := readyTuner(pool).coolingErr(ctx, qid); !errors.Is(err, ErrHintExists) {
		t.Fatalf("dormant hint not counted as covered: %v", err)
	}

	runProbe(t, ctx, pool, revalPassesCols, 1)
	mustRevalidate(t, ctx, tu)
	if s := readHintState(t, ctx, pool, id); s.status != "active" {
		t.Fatalf("after the query ran again: status = %q, want active", s.status)
	}
}

func assertInstalledAndKept(t *testing.T, ctx context.Context, tu *Tuner, qid int64) {
	t.Helper()
	var n int
	if err := tu.pool.QueryRow(ctx,
		"SELECT count(*) FROM hint_plan.hints WHERE query_id = $1", qid).Scan(&n); err != nil {
		t.Fatalf("count installed hints: %v", err)
	}
	if n != 1 {
		t.Fatalf("hint_plan.hints rows for %d = %d, want 1", qid, n)
	}
	for _, f := range tu.hintRemovalFindings(ctx) {
		if f.ObjectIdentifier == fmt.Sprintf("queryid:%d", qid) {
			t.Fatalf("removal finding emitted for a kept hint: %s", f.Title)
		}
	}
}

// A missing pg_stat_statements_info is a read error: the hint keeps its
// status and nothing is written. The fixture hid the view behind
// search_path = sage until security-01c-q, which finds the extension by its
// catalog schema whatever the search_path; the database here has none.
func TestRevalidate_MissingPgssInfoFailsClosed(t *testing.T) {
	_, ctx := requireTunerDB(t)
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "tunernopgss"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop pg_stat_statements: %v", err)
	}
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	id := seedProbeHint(t, ctx, pool, revalTestQIDBase+500, `Set(work_mem "64MB")`)

	obs, err := tu.fetchQueryStats(ctx, revalTestQIDBase+500)
	if err == nil || !strings.Contains(err.Error(), "pg_stat_statements") {
		t.Fatalf("fetchQueryStats = %+v, %v; want an error naming pg_stat_statements",
			obs, err)
	}
	rpt := mustRevalidate(t, ctx, tu)
	if rpt.Errors < 1 {
		t.Fatalf("report = %+v, want at least one error", rpt)
	}
	s := readHintState(t, ctx, pool, id)
	var revalidated *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_revalidated_at FROM sage.query_hints "+
		"WHERE id = $1", id).Scan(&revalidated); err != nil {
		t.Fatalf("read last_revalidated_at: %v", err)
	}
	if s.status != "active" || s.checkpoint != nil || s.resetAt != nil || revalidated != nil {
		t.Fatalf("hint after a failed read = %+v revalidated=%v, want untouched", s, revalidated)
	}
}

// Revalidate and Tune run concurrently on one Tuner; t.mu serializes them.
// (-race is unavailable on this machine; this still catches deadlocks and
// panics, and the final state must be a valid one.)
func TestRevalidate_ConcurrentWithTune(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{MinQueryCalls: 1 << 30}, nil, noopLogFn)
	id := seedProbeHint(t, ctx, pool, revalTestQIDBase+501, `Set(work_mem "64MB")`)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := tu.Revalidate(ctx); err != nil {
				errs <- fmt.Errorf("revalidate: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := tu.Tune(ctx, nil); err != nil {
				errs <- fmt.Errorf("tune: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if s := readHintState(t, ctx, pool, id); s.status != "dormant" {
		t.Fatalf("absent-queryid hint = %q after concurrent passes, want dormant", s.status)
	}
}
