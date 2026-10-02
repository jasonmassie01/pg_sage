package analyzer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// No parallel fixtures: collection and analysis ordering is the subject of these tests.
func preflightPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	p, err := pgxpool.New(context.Background(), os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	ctx := context.Background()
	if err = p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err = schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p, ctx
}

func preflightSQL(t *testing.T, p *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func preflightCollector(t *testing.T, p *pgxpool.Pool) (
	*collector.Collector, *config.Config, <-chan string,
) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 1
	cfg.Analyzer.SlowQueryThresholdMs = 1
	// The collector keeps the top max_queries statements by total time; in
	// a full parallel suite the fixture's short workload fell out of the
	// default window and the test flaked.
	cfg.Collector.MaxQueries = 50000
	cfg.Advisor.Enabled = false
	cfg.Safety.QueryTimeoutMs = 5000
	logs := make(chan string, 100)
	logFn := func(level, format string, args ...any) {
		select {
		case logs <- fmt.Sprintf(format, args...):
		default:
		}
	}
	c := collector.New(p, cfg, 170000, logFn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for c.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c.LatestSnapshot() == nil {
		t.Fatal("collector produced no real snapshot")
	}
	return c, cfg, logs
}

// preflightCapture waits until a collected snapshot shows the workload
// (marker in its text) and returns its queryid. pg_stat_statements is
// cluster-wide: another package's unscoped pg_stat_statements_reset()
// can wipe the entry between the workload and the first snapshot, so the
// workload runs again until a snapshot has it.
func preflightCapture(t *testing.T, p *pgxpool.Pool, c *collector.Collector,
	sql, marker string) int64 {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if snap := c.LatestSnapshot(); snap != nil {
			for _, q := range snap.Queries {
				if strings.Contains(q.Query, marker) {
					return q.QueryID
				}
			}
		}
		preflightSQL(t, p, sql)
		time.Sleep(1100 * time.Millisecond) // one collector interval
	}
	t.Fatal("missing slow workload: no snapshot captured it in 20 s")
	return 0
}

// preflightWaitFailure waits for the collector to report that it could
// not read pg_stat_statements. Since dogfood lifeos-1 a failed category
// no longer fails the snapshot: the snapshot is kept with the queries
// marked unavailable.
func preflightWaitFailure(t *testing.T, logs <-chan string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-logs:
			if strings.Contains(line, "queries unavailable") {
				t.Log(line)
				return
			}
		case <-timer.C:
			t.Fatal("collector failure not observed")
			return
		}
	}
}

// A cycle without fresh query evidence (here: pg_stat_statements
// dropped, so the snapshot's queries are unavailable) must not refresh a
// slow_query finding's occurrence count or last_seen.
func TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding(t *testing.T) {
	p, ctx := preflightPool(t)
	preflightSQL(t, p, "SELECT pg_sleep(0.02)")
	c, cfg, logs := preflightCollector(t, p)
	a := New(p, cfg, c, nil, nil, nil, nil, func(string, string, ...any) {})
	var beforeCount, afterCount int
	var beforeSeen, afterSeen time.Time
	sql := `SELECT occurrence_count,last_seen FROM sage.findings
		WHERE category='slow_query' AND object_identifier=$1 AND status='open'`
	// pg_stat_statements is cluster-wide: another package may reset it
	// between capture and cycle, so capture and analyze until the
	// finding exists.
	var ident string
	for attempt := 0; ; attempt++ {
		qid := preflightCapture(t, p, c, "SELECT pg_sleep(0.02)", "pg_sleep")
		a.cycle(ctx)
		ident = fmt.Sprintf("queryid:%d", qid)
		err := p.QueryRow(ctx, sql, ident).Scan(&beforeCount, &beforeSeen)
		if err == nil {
			break
		}
		if attempt == 4 {
			t.Fatalf("no slow_query finding after 5 captures: %v", err)
		}
	}
	preflightSQL(t, p, "DROP EXTENSION pg_stat_statements")
	t.Cleanup(func() { preflightSQL(t, p, "CREATE EXTENSION pg_stat_statements") })
	preflightWaitFailure(t, logs)
	for deadline := time.Now().Add(5 * time.Second); c.LatestSnapshot().Available("queries") &&
		time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if c.LatestSnapshot().Available("queries") {
		t.Fatal("no snapshot with the queries unavailable")
	}
	a.cycle(ctx)
	if err := p.QueryRow(ctx, sql, ident).Scan(&afterCount, &afterSeen); err != nil {
		t.Fatal(err)
	}
	t.Logf("count=%d->%d last_seen=%v->%v", beforeCount, afterCount, beforeSeen, afterSeen)
	if afterCount != beforeCount || !afterSeen.Equal(beforeSeen) {
		t.Error("failed collection let stale evidence refresh finding occurrence and last_seen")
	}
}

func TestPreflightEvidencePercentCollectorToRatioRule(t *testing.T) {
	p, ctx := preflightPool(t)
	preflightSQL(t, p, "CREATE SCHEMA preflight_catalog")
	preflightSQL(t, p, `CREATE VIEW preflight_catalog.pg_stat_database AS
		SELECT datname,80::bigint blks_hit,20::bigint blks_read,
		deadlocks,blk_read_time,blk_write_time FROM pg_catalog.pg_stat_database`)
	pcfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = "preflight_catalog,pg_catalog,public"
	shadow, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shadow.Close)
	c, cfg, _ := preflightCollector(t, shadow)
	snap := c.LatestSnapshot()
	findings := ruleCacheHitRatio(snap, nil, cfg, nil)
	t.Logf("catalog input hit=80 read=20; collected=%f threshold=%f findings=%d",
		snap.System.CacheHitRatio, cfg.Analyzer.CacheHitRatioWarning, len(findings))
	if len(findings) != 1 {
		t.Error("real collector-to-rule path missed 80% low-cache warning")
	}
}

func TestPreflightEvidenceZeroExecPlanningPersistence(t *testing.T) {
	p, ctx := preflightPool(t)
	cfg := config.DefaultConfig()
	for _, calls := range []int64{0, 100} {
		t.Run(fmt.Sprintf("calls_%d", calls), func(t *testing.T) {
			snap := &collector.Snapshot{Queries: []collector.QueryStats{{
				QueryID: 7654321, Calls: calls, MeanPlanTime: 2, MeanExecTime: 0,
			}}}
			findings := ruleHighPlanTime(snap, nil, cfg, nil)
			if calls == 0 && len(findings) != 0 {
				t.Error("zero executions should not create ratio")
			}
			err := UpsertFindings(ctx, p, findings)
			t.Logf("synthetic nonnegative input calls=%d findings=%d persistence=%v",
				calls, len(findings), err)
			if err != nil {
				t.Errorf("zero denominator produced unpersistable finding: %v", err)
			}
		})
	}
}

func TestPreflightEvidenceFailedExecutionsDoNotCreatePlanningFinding(t *testing.T) {
	p, ctx := preflightPool(t)
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SET pg_stat_statements.track_planning=on"); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		_, err = conn.Exec(ctx, "SELECT 1 / i FROM generate_series(0,0) i")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
			t.Fatalf("expected division-by-zero execution failure; got %v", err)
		}
	}
	var q collector.QueryStats
	var plans int64
	err = conn.QueryRow(ctx, `SELECT queryid,calls,plans,mean_plan_time,mean_exec_time
		FROM pg_stat_statements WHERE query LIKE $1`,
		"SELECT % / i FROM generate_series% i").Scan(
		&q.QueryID, &q.Calls, &plans, &q.MeanPlanTime, &q.MeanExecTime)
	if err != nil {
		t.Fatal(err)
	}
	if plans < 1 || q.Calls != 0 || q.MeanPlanTime <= 0 || q.MeanExecTime != 0 {
		t.Fatalf("unexpected failed-execution source row: plans=%d query=%+v", plans, q)
	}
	snap := &collector.Snapshot{Queries: []collector.QueryStats{q}}
	findings := ruleHighPlanTime(snap, nil, config.DefaultConfig(), nil)
	if len(findings) != 0 {
		t.Fatalf("failed execution generated planning findings: %v", findings)
	}
	if err = UpsertFindings(ctx, p, findings); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual pg_stat_statements plans=%d calls=%d plan_ms=%f exec_ms=%f findings=0",
		plans, q.Calls, q.MeanPlanTime, q.MeanExecTime)
}
