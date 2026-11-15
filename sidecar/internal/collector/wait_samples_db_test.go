package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// Wait-event load (market-01): one aggregate read per cycle of this
// database's active client sessions by wait class, without pg_sage's own
// sessions, query text or pids.

func waitTotal(rows []WaitEventLoad) (int, map[string]int) {
	total, byType := 0, map[string]int{}
	for _, r := range rows {
		total += r.Sessions
		byType[r.WaitEventType+"/"+r.WaitEvent] += r.Sessions
	}
	return total, byType
}

// otherDatabaseSleeper runs pg_sleep on another database of the server
// (created once) until the test ends: a session the read must not count.
func otherDatabaseSleeper(t *testing.T, dsn string) {
	t.Helper()
	const other = "market01_other"
	admin := appSession(t, dsn, "postgres_same")
	_, err := admin.Exec(context.Background(), "CREATE DATABASE "+other)
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "42P04") {
		t.Fatalf("create %s: %v", other, err)
	}
	conn := appSession(t, dsn, other)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(ctx, "SELECT pg_sleep(600) AS other_db_sleep")
	}()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(300 * time.Millisecond)
}

func TestWaitEvents_CountThisDatabaseApplicationSessionsOnly(t *testing.T) {
	c, _ := sageCollector(t)
	dsn := testdb.SkipUnlessLive(t)
	selfload.Start(t, dsn) // application and pg_sage: a waiter and a sleeper each
	otherDatabaseSleeper(t, dsn)
	rows, err := c.collectWaitEvents(context.Background())
	if err != nil {
		t.Fatalf("collectWaitEvents: %v", err)
	}
	total, byType := waitTotal(rows)
	if total != 2 {
		t.Fatalf("sessions %d (%v), want 2: the application waiter and sleeper only",
			total, byType)
	}
	if byType["Lock/relation"] != 1 || byType["Timeout/PgSleep"] != 1 {
		t.Errorf("by wait %v, want Lock/relation 1 and Timeout/PgSleep 1", byType)
	}
	for _, r := range rows {
		if r.WaitEventType == "" || r.Sessions <= 0 {
			t.Errorf("row %+v: want a wait class and a positive count", r)
		}
	}
}

// A session running on CPU has no wait event: it is counted as CPU, with
// no event name.
func TestWaitEvents_RunningSessionIsCPU(t *testing.T) {
	c, _ := sageCollector(t)
	busy := appSession(t, testdb.SkipUnlessLive(t), "postgres_same")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = busy.Exec(ctx, "DO $$BEGIN LOOP PERFORM 1; END LOOP; END$$")
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(500 * time.Millisecond)
	rows, err := c.collectWaitEvents(context.Background())
	if err != nil {
		t.Fatalf("collectWaitEvents: %v", err)
	}
	_, byType := waitTotal(rows)
	if byType["CPU/"] != 1 {
		t.Fatalf("by wait %v, want CPU/ 1 for the busy session", byType)
	}
}

// appSession connects an application session; db "postgres_same" keeps
// the test database.
func appSession(t *testing.T, dsn, db string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if db != "postgres_same" {
		cfg.Database = db
	}
	cfg.RuntimeParams["application_name"] = selfload.AppName
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestWaitEvents_IdleDatabaseIsAnEmptyList(t *testing.T) {
	c, _ := sageCollector(t)
	rows, err := c.collectWaitEvents(context.Background())
	if err != nil {
		t.Fatalf("collectWaitEvents: %v", err)
	}
	if rows == nil {
		t.Fatal("an idle database read as nil, want an empty list (known zero, not unknown)")
	}
}

func TestWaitEvents_ReadFailureIsUnavailable(t *testing.T) {
	c, _ := sageCollector(t)
	c.overrideStep("wait_events", func(context.Context, *Snapshot) error {
		return errors.New("injected: permission denied for view pg_stat_activity")
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.Available("wait_events") || snap.WaitEvents != nil {
		t.Fatalf("failed read: available %v rows %v, want unknown (no rows, unavailable)",
			snap.Available("wait_events"), snap.WaitEvents)
	}
}

// statementCalls is the calls pg_stat_statements counts for statements
// whose text contains marker, in this database.
func statementCalls(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, marker string) int64 {
	t.Helper()
	view, err := extschema.StatementsRel(ctx, q, "pg_stat_statements")
	if err != nil {
		t.Skipf("pg_stat_statements not installed: %v", err)
	}
	var n int64
	err = q.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0)::bigint FROM `+view+`
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND strpos(query, $1) > 0`, marker).Scan(&n)
	if err != nil {
		t.Skipf("pg_stat_statements not readable: %v", err)
	}
	return n
}

// CHECK-03: the explorer adds exactly one aggregate statement per cycle.
func TestWaitEvents_OneStatementPerCycle(t *testing.T) {
	c, pool := sageCollector(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	before := statementCalls(t, ctx, pool, waitLoadTag)
	if _, err := c.collect(ctx); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := c.collect(ctx); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := statementCalls(t, ctx, pool, waitLoadTag) - before; got != 2 {
		t.Fatalf("wait-load statements in two cycles = %d, want 2 (one per cycle)", got)
	}
}

// The cycle stores the reading as the wait_events snapshot category.
func TestWaitEvents_PersistedWithTheSnapshot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	start := time.Now().Add(-time.Second)
	if err := c.cycle(ctx, &fakeTicker{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	var data []byte
	err := pool.QueryRow(ctx, `SELECT data::text FROM sage.snapshots
		WHERE category = 'wait_events' AND collected_at >= $1
		ORDER BY collected_at DESC LIMIT 1`, start).Scan(&data)
	if err != nil {
		t.Fatalf("read the stored wait_events row: %v", err)
	}
	var rows []WaitEventLoad
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		t.Fatalf("stored document %s: %v, want a JSON list", data, err)
	}
	if strings.Contains(string(data), "query") || strings.Contains(string(data), "pid") {
		t.Errorf("stored document %s carries query text or pids", data)
	}
}

// Integration: a workload between two cycles is the window's top query.
func TestWorkloadExplorer_TwoCyclesRankTheWorkloadFirst(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	runWorkload := func(n int) {
		for range n {
			if _, err := pool.Exec(ctx,
				"SELECT pg_sleep(0.05) AS explorer_workload_market01"); err != nil {
				t.Fatalf("workload: %v", err)
			}
		}
	}
	// The workload runs before the first cycle too, as a running
	// application does: a query's first sample has nothing to differ from.
	runWorkload(2)
	if err := c.cycle(ctx, &fakeTicker{}); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	// The first cycle's samples are the anchors: work done before it
	// (earlier tests) is not this window's. The window is read on the
	// database clock, which stamps captured_at: the test server's VM clock
	// can lead the host's by more than the second of slack below.
	from := testdb.MustDBNow(t, pool)
	runWorkload(30)
	if err := c.cycle(ctx, &fakeTicker{}); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	var qid int64
	if err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE strpos(query, 'explorer_workload_market01') > 0
		  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		LIMIT 1`).Scan(&qid); err != nil {
		t.Skipf("pg_stat_statements has no workload entry: %v", err)
	}
	res, err := querystore.Top(ctx, pool, querystore.TopRequest{From: from,
		To: testdb.MustDBNow(t, pool).Add(time.Second), Order: querystore.OrderLoad,
		Limit: 5})
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if len(res.Queries) == 0 || res.Queries[0].QueryID != qid {
		t.Fatalf("top %+v, want the workload's queryid %d first", res.Queries, qid)
	}
	if got := res.Queries[0].Calls; got != 30 {
		t.Errorf("workload calls in the window = %d, want 30", got)
	}
}
