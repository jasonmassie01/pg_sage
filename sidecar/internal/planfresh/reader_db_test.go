package planfresh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/planfresh"))
}

var (
	bootOnce sync.Once
	bootErr  error
	seq      atomic.Int64
)

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("no test PostgreSQL (%s): %v", testdb.EnvName, err)
	}
	t.Cleanup(pool.Close)
	bootOnce.Do(func() { bootErr = schema.Bootstrap(context.Background(), pool) })
	if bootErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootErr)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// fixture is a fresh schema with an analyzed table t and a queryid of its
// own; cleanup drops both.
func fixture(t *testing.T, pool *pgxpool.Pool) (string, int64) {
	t.Helper()
	n := seq.Add(1)
	s := fmt.Sprintf("pf_%d_%d", time.Now().UnixNano()%1_000_000_000, n)
	qid := -(time.Now().UnixNano() + n)
	exec(t, pool, "CREATE SCHEMA "+s)
	exec(t, pool, "CREATE TABLE "+s+".t (id int PRIMARY KEY, v int)")
	exec(t, pool, "INSERT INTO "+s+".t SELECT g, g FROM generate_series(1, 200) g")
	exec(t, pool, "ANALYZE "+s+".t")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
		_, _ = pool.Exec(bg, "DROP SCHEMA IF EXISTS "+s+" CASCADE")
	})
	return s, qid
}

// analyzeAndWait runs ANALYZE on s.t and waits until another session sees
// it (PG14's statistics collector reports with a delay).
func analyzeAndWait(t *testing.T, pool *pgxpool.Pool, s string) {
	t.Helper()
	var mark time.Time
	err := pool.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&mark)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	exec(t, pool, "ANALYZE "+s+".t")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var seen bool
		if err := pool.QueryRow(context.Background(), `SELECT COALESCE(last_analyze > $2, false)
			FROM pg_stat_user_tables WHERE schemaname = $1 AND relname = 't'`, s, mark).
			Scan(&seen); err == nil && seen {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ANALYZE of %s.t never became visible", s)
}

func planOn(s string) string {
	return `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Schema":"` + s + `"}}]`
}

func cache(t *testing.T, pool *pgxpool.Pool, qid int64, ago, plan, source string) {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.explain_cache (queryid, captured_at, plan_json, source)
		VALUES ($1, now() - $2::interval, $3, $4)`, qid, ago, plan, source)
}

func reader(pool *pgxpool.Pool) Reader {
	return Reader{DB: catalogread.New(pool, catalogread.Default())}
}

func TestReader_FreshCachedPlanIsUsed(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain")
	before := time.Now()
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceCache || p.Capture != "auto_explain" ||
		!strings.Contains(string(p.JSON), "Seq Scan") || p.HasActuals {
		t.Fatalf("fresh cached plan = %+v, %v", p, err)
	}
	if d := p.CapturedAt.Sub(before); d < -time.Minute || d > time.Minute {
		t.Fatalf("captured_at must be the row's (just now): %v vs %v", p.CapturedAt, before)
	}
}

func TestReader_AnalyzeAfterCaptureMakesItStale(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain")
	analyzeAndWait(t, pool, s)
	p, err := reader(pool).Read(context.Background(), qid, "SELECT 1")
	if err != nil || p.Source != SourceNone || len(p.JSON) != 0 ||
		!strings.Contains(p.StaleReason, "ANALYZE") {
		t.Fatalf("a plan older than ANALYZE must not be used: %+v, %v", p, err)
	}
}

func TestReader_PgSageActionAfterCaptureMakesItStale(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain")
	var findingID int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail)
		VALUES ('missing_index', 'warning', 'table', $1, 'x', '{}') RETURNING id`,
		s+".t").Scan(&findingID); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	exec(t, pool, `INSERT INTO sage.action_log (action_type, finding_id, sql_executed, outcome)
		VALUES ('create_index', $1, 'CREATE INDEX ...', 'failed')`, findingID)
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceCache {
		t.Fatalf("a failed action changes nothing; the plan stays fresh: %+v, %v", p, err)
	}
	exec(t, pool, `INSERT INTO sage.action_log (action_type, finding_id, sql_executed, outcome)
		VALUES ('create_index', $1, 'CREATE INDEX ...', 'success')`, findingID)
	p, err = reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceNone || !strings.Contains(p.StaleReason, "pg_sage") {
		t.Fatalf("a pg_sage CREATE INDEX after capture must make it stale: %+v, %v", p, err)
	}
}

func TestReader_StaleCacheFallsBackToOneExplain(t *testing.T) {
	pool := dbPool(t)
	_, qid := fixture(t, pool)
	cache(t, pool, qid, "30 hours", `[{"Plan":{"Node Type":"Index Scan"}}]`, "auto_explain")
	var calls int
	r := reader(pool)
	r.Explain = func(_ context.Context, text string) (Plan, error) {
		calls++
		return Plan{Source: SourceExplain, JSON: []byte(`[{"Plan":{"Actual Rows":1}}]`)}, nil
	}
	p, err := r.Read(context.Background(), qid, "SELECT 1")
	if err != nil || calls != 1 || p.Source != SourceExplain || p.HasActuals ||
		p.CapturedAt.IsZero() || !strings.Contains(p.StaleReason, "old") {
		t.Fatalf("stale cache -> one plain EXPLAIN without actuals: %+v, %v (calls %d)",
			p, err, calls)
	}
}

func TestReader_FailingExplainNeverReturnsTheStalePlan(t *testing.T) {
	pool := dbPool(t)
	_, qid := fixture(t, pool)
	cache(t, pool, qid, "30 hours", `[{"Plan":{"Node Type":"Index Scan"}}]`, "auto_explain")
	r := reader(pool)
	r.Explain = func(context.Context, string) (Plan, error) {
		return Plan{}, errors.New("relation does not exist")
	}
	p, err := r.Read(context.Background(), qid, "SELECT * FROM missing")
	if err == nil || !strings.Contains(err.Error(), "does not exist") || len(p.JSON) != 0 {
		t.Fatalf("a failing EXPLAIN must be refused, not answered with the stale plan: %+v %v",
			p, err)
	}
}

func TestReader_OlderFreshCaptureWinsOverAnUnreadableNewest(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain_log")
	cache(t, pool, qid, "-1 second", `{"not":"a plan"}`, "auto_explain")
	cache(t, pool, qid, "-2 seconds", `[{"Plan":{"Node Type":"Foreign Scan"}}]`,
		"lakebase_plan_history")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Capture != "auto_explain_log" || !strings.Contains(string(p.JSON), s) {
		t.Fatalf("the newest fresh local capture must be used: %+v, %v", p, err)
	}
}

func TestReader_NoCacheExplainsOnlyWhenAsked(t *testing.T) {
	pool := dbPool(t)
	_, qid := fixture(t, pool)
	var calls int
	r := reader(pool)
	r.Explain = func(context.Context, string) (Plan, error) {
		calls++
		return Plan{Source: SourceExplain, JSON: []byte(`[]`)}, nil
	}
	p, err := r.Read(context.Background(), qid, "SELECT 1")
	if err != nil || calls != 0 || p.Source != SourceNone {
		t.Fatalf("no cache row and ExplainUncached off: no EXPLAIN: %+v %v (%d)", p, err, calls)
	}
	r.ExplainUncached = true
	if p, err = r.Read(context.Background(), qid, "SELECT 1"); err != nil || calls != 1 ||
		p.Source != SourceExplain {
		t.Fatalf("ExplainUncached plans afresh: %+v %v (%d)", p, err, calls)
	}
}

// failingStats fails the relation-events read, passing everything else.
type failingStats struct{ catalogread.Querier }

func (f failingStats) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "pg_stat_user_tables") {
		return errRow{errors.New("permission denied for view pg_stat_user_tables")}
	}
	return f.Querier.QueryRow(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func TestReader_StatsReadErrorIsStale(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain")
	r := Reader{DB: failingStats{catalogread.New(pool, catalogread.Default())}}
	p, err := r.Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceNone || !strings.Contains(p.StaleReason, "permission") {
		t.Fatalf("an unreadable pg_stat_user_tables must count as stale: %+v %v", p, err)
	}
}

func TestReader_CacheReadErrorPropagates(t *testing.T) {
	pool := dbPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := reader(pool).Read(ctx, 1, "")
	if err == nil || !strings.Contains(err.Error(), "explain cache") {
		t.Fatalf("a failed cache read must name what failed: %v", err)
	}
}

func TestReader_ConcurrentReadsAgree(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "auto_explain")
	r := reader(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := r.Read(context.Background(), qid, "")
			if err == nil && p.Source != SourceCache {
				err = fmt.Errorf("source %q", p.Source)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	}
}

func TestReader_DecisionTargetNamingTheIndexMakesItStale(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", `[{"Plan":{"Node Type":"Index Scan","Relation Name":"t",`+
		`"Schema":"`+s+`","Index Name":"t_pkey"}}]`, "auto_explain")
	var actionID int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('drop_index', 'DROP INDEX x', 'success')
		RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.decision WHERE action_log_id = $1", actionID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE id = $1", actionID)
	})
	exec(t, pool, `INSERT INTO sage.decision (feature, intent, target_objects, verdict,
		risk_tier, reason, evidence_id, action_log_id)
		VALUES ('index', 'drop_index', jsonb_build_array($1::text), 'execute', 'safe', 'r',
		'ev-tuning08', $2)`, s+`."T_PKEY"`, actionID)
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceNone || !strings.Contains(p.StaleReason, "pg_sage") {
		t.Fatalf("a decision targeting the plan's index must make it stale: %+v, %v", p, err)
	}
}
