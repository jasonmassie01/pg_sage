package perfgate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "testsupport_perfgate"))
}

// tinyScale keeps the live tests fast: 3 schemas (one clone) of 4 tables.
func tinyScale() Scale {
	return Scale{Name: "tiny", Schemas: 3, CloneSchemas: 1, TablesPerSchema: 4,
		IndexesPerTable: 3, HistoryRows: 300}
}

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "perfgate_lib")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func count(t *testing.T, ctx context.Context, q Querier, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestBuildCatalogCreatesTheScaledCatalog(t *testing.T) {
	pool, ctx := livePool(t)
	s := tinyScale()
	for run := 0; run < 2; run++ { // the second run must be a no-op
		if err := BuildCatalog(ctx, pool, s); err != nil {
			t.Fatalf("build run %d: %v", run, err)
		}
	}
	const user = `n.nspname LIKE 'perf\_%'`
	tables := count(t, ctx, pool, `SELECT count(*) FROM pg_class c JOIN pg_namespace n
		ON n.oid = c.relnamespace WHERE c.relkind = 'r' AND `+user)
	indexes := count(t, ctx, pool, `SELECT count(*) FROM pg_class c JOIN pg_namespace n
		ON n.oid = c.relnamespace WHERE c.relkind = 'i' AND `+user)
	seqs := count(t, ctx, pool, `SELECT count(*) FROM pg_class c JOIN pg_namespace n
		ON n.oid = c.relnamespace WHERE c.relkind = 'S' AND `+user)
	if tables != int64(s.Tables()) || indexes != int64(s.Indexes()) ||
		seqs != int64(s.Sequences()) {
		t.Fatalf("catalog = %d/%d/%d, want %d/%d/%d", tables, indexes, seqs,
			s.Tables(), s.Indexes(), s.Sequences())
	}
	// Clone schemas are identical copies: same table names as each other.
	clones := count(t, ctx, pool, `SELECT count(DISTINCT c.relname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname LIKE 'perf\_clone\_%'`)
	if clones != int64(s.TablesPerSchema) {
		t.Fatalf("clone schemas hold %d distinct tables, want %d", clones, s.TablesPerSchema)
	}
	// The hot tables carry rows so the workload and statistics are real.
	if rows := count(t, ctx, pool, `SELECT count(*) FROM perf_app_000.t_0000`); rows == 0 {
		t.Fatal("hot table is empty")
	}
}

func TestBuildCatalogRejectsInvalidScale(t *testing.T) {
	pool, ctx := livePool(t)
	err := BuildCatalog(ctx, pool, Scale{})
	if err == nil || !strings.Contains(err.Error(), "scale") {
		t.Fatalf("invalid scale: err = %v", err)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM pg_namespace
		WHERE nspname LIKE 'perf\_%'`); n != 0 {
		t.Fatalf("invalid scale created %d schemas", n)
	}
}

func seededPool(t *testing.T) (*pgxpool.Pool, context.Context, Binding) {
	t.Helper()
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	own := NewBinding("startup:perfgate")
	if err := SeedHistory(ctx, pool, tinyScale(), own); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool, ctx, own
}

func TestSeedHistoryFillsEveryGrowingTable(t *testing.T) {
	pool, ctx, own := seededPool(t)
	s := tinyScale()
	for _, table := range GrowingTables() {
		n := count(t, ctx, pool, "SELECT count(*) FROM "+pgx.Identifier{"sage", table}.Sanitize())
		if n < int64(s.HistoryRows) {
			t.Errorf("sage.%s has %d rows, want >= %d", table, n, s.HistoryRows)
		}
	}
	// Snapshot history is in the keyframe+delta format and every row reads back.
	deltas := count(t, ctx, pool, `SELECT count(*) FROM sage.snapshots WHERE base_id IS NOT NULL`)
	unreadable := count(t, ctx, pool, `SELECT count(*) FROM sage.snapshots
		WHERE sage.snapshot_data(data, base_id) IS NULL`)
	if deltas == 0 || unreadable != 0 {
		t.Fatalf("snapshots: %d deltas, %d unreadable", deltas, unreadable)
	}
	// Most of the SRE history belongs to the runtime's own binding.
	mine := count(t, ctx, pool, `SELECT count(*) FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2`, own.DeploymentID, own.DatabaseID)
	all := count(t, ctx, pool, `SELECT count(*) FROM sage.sre_investigations`)
	if mine == 0 || mine == all {
		t.Fatalf("own investigations %d of %d; want a majority, not all", mine, all)
	}
	key := count(t, ctx, pool, `SELECT count(*) FROM sage.sre_database_bindings
		WHERE runtime_key = $1 AND database_id = $2`, own.RuntimeKey, own.DatabaseID)
	if key != 1 {
		t.Fatalf("own binding rows = %d", key)
	}
	// Ledger history spans many families, as in a real fleet: one family
	// would make every per-family index useless to the planner.
	families := count(t, ctx, pool, `SELECT count(DISTINCT family)
		FROM sage.sre_autonomy_outcomes`)
	if families < 10 {
		t.Fatalf("autonomy outcomes span %d families, want >= 10", families)
	}
	// History is history: nothing seeded is live work the runtime would pick up.
	live := count(t, ctx, pool, `SELECT count(*) FROM sage.sre_investigations
		WHERE state IN ('queued','collecting','evaluating','needs_evidence','paused')`)
	if live != 0 {
		t.Fatalf("%d live investigations seeded", live)
	}
}

func TestSeedHistoryNeedsTheSageSchema(t *testing.T) {
	pool, ctx := livePool(t)
	err := SeedHistory(ctx, pool, tinyScale(), NewBinding("startup:x"))
	if err == nil || !strings.Contains(err.Error(), "sage") {
		t.Fatalf("seed without schema: err = %v", err)
	}
}

func TestTableStatsDeltaSeesSeqScanAndWrites(t *testing.T) {
	pool, ctx, _ := seededPool(t)
	if err := AnalyzeSage(ctx, pool); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	before, err := ReadTableStats(ctx, pool)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SET enable_indexscan = off", "SET enable_bitmapscan = off",
		"SET enable_indexonlyscan = off", "SELECT count(*) FROM sage.decision WHERE id > 0",
		"UPDATE sage.findings SET last_seen = now() WHERE id <= 5"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	conn.Hijack().Close(ctx) // backend exit flushes its statistics
	var d TableDelta
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		after, err := ReadTableStats(ctx, pool)
		if err != nil {
			t.Fatalf("after: %v", err)
		}
		d = deltaFor(after.Delta(before), "sage.decision")
		if d.SeqScans > 0 && deltaFor(after.Delta(before), "sage.findings").RowsWritten >= 5 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.SeqScans < 1 || d.SeqTupRead < int64(tinyScale().HistoryRows) {
		t.Fatalf("decision delta = %+v, want a seq scan over the seeded rows", d)
	}
	if d.LiveRows < int64(tinyScale().HistoryRows) {
		t.Fatalf("live rows = %d", d.LiveRows)
	}
}

func deltaFor(ds []TableDelta, name string) TableDelta {
	for _, d := range ds {
		if d.Name == name {
			return d
		}
	}
	return TableDelta{}
}

func TestStatementsCaptureAndExplain(t *testing.T) {
	pool, ctx, _ := seededPool(t)
	if err := Prepare(ctx, pool); err != nil {
		if errors.Is(err, ErrStatementsUnavailable) {
			t.Skipf("pg_stat_statements not preloaded: %v", err)
		}
		t.Fatalf("prepare: %v", err)
	}
	if err := AnalyzeSage(ctx, pool); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var found *Statement
	// Another package's pg_stat_statements_reset() or an eviction on the
	// shared server can erase the entry between the call and the read: such
	// a window (the epoch moved after this test's own reset) is repeated.
	for attempt := 1; ; attempt++ {
		problem, moved := captureOneCall(t, ctx, pool, &found)
		if problem == "" {
			break
		}
		if !moved || attempt == 3 {
			t.Fatal(problem)
		}
		t.Logf("attempt %d: pg_stat_statements was reset or evicted; repeating", attempt)
	}
	checkExplain(t, ctx, pool, *found)
}

// captureOneCall resets the statements, runs one tagged and one untagged
// statement and reads them back. It returns the problem found, if any, and
// whether the statistics changed generation during the window.
func captureOneCall(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	found **Statement) (string, bool) {
	t.Helper()
	if err := ResetStatements(ctx, pool); err != nil {
		t.Fatalf("reset: %v", err)
	}
	before, err := pgssepoch.Epoch(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "SELECT count(*) FROM sage.decision WHERE reason = $1",
		"x"); err != nil {
		t.Fatal(err)
	}
	tagged := "/* " + HarnessTag + " */ SELECT count(*) FROM sage.findings"
	if _, err := pool.Exec(ctx, tagged); err != nil {
		t.Fatal(err)
	}
	var db string
	_ = pool.QueryRow(ctx, "SELECT current_database()").Scan(&db)
	stmts, err := ReadStatements(ctx, pool, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	after, err := pgssepoch.Epoch(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	*found = nil
	for i := range stmts {
		if strings.Contains(stmts[i].Query, HarnessTag) {
			t.Fatalf("harness statement captured: %q", stmts[i].Query)
		}
		if strings.Contains(stmts[i].Query, "sage.decision WHERE reason") {
			*found = &stmts[i]
		}
	}
	f := *found
	if f == nil || f.Calls != 1 || f.QueryID == 0 || f.TotalMs <= 0 {
		return fmt.Sprintf("captured statement = %+v among %d", f, len(stmts)),
			before != after
	}
	return "", false
}

func checkExplain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scan Statement) {
	t.Helper()
	byID := Statement{QueryID: 2, Query: "SELECT * FROM sage.decision WHERE id = $1"}
	// A statement on a relation that is gone (a dropped or temporary table)
	// cannot be planned afterwards.
	missing := Statement{QueryID: 3, Query: "SELECT * FROM sage.perfgate_gone WHERE id = $1"}
	utility := Statement{QueryID: 4, Query: "SET statement_timeout = $1"}
	res, err := ExplainStatements(ctx, pool, []Statement{scan, byID, missing, utility})
	if errors.Is(err, ErrGenericPlanUnsupported) {
		if count(t, ctx, pool, "SELECT current_setting('server_version_num')::int") >= 160000 {
			t.Fatalf("generic plans reported unsupported on PG16+: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("results = %+v; the utility statement must be skipped", res)
	}
	if len(res[0].SeqScans) != 1 || res[0].SeqScans[0].Relation != "decision" {
		t.Fatalf("unindexed filter plan = %+v", res[0])
	}
	if len(res[1].SeqScans) != 0 || res[1].Err != "" {
		t.Fatalf("primary key lookup plan = %+v", res[1])
	}
	if res[2].Err == "" {
		t.Fatalf("missing relation explained without error: %+v", res[2])
	}
}
