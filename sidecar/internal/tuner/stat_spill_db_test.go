package tuner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-03 on real PostgreSQL (14 and 17): the baseline work_mem is the
// statement role's, block_size comes from the server, and a real spilling
// statement gets a hint sized per call from the interval between cycles.

// spillRole creates a login role for the test and drops it afterwards.
func spillRole(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP ROLE IF EXISTS "+name); err != nil {
		t.Fatalf("drop stale role: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE ROLE "+name+" LOGIN"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+name) })
	var oid int64
	if err := pool.QueryRow(ctx, "SELECT oid::int8 FROM pg_roles WHERE rolname = $1",
		name).Scan(&oid); err != nil {
		t.Fatalf("role oid: %v", err)
	}
	return oid
}

func settingKB(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(), sql).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	kb, err := parseMemoryKB(v)
	if err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return kb
}

func execAll(t *testing.T, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func serverBlockSize(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var bs int64
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('block_size')::int8").Scan(&bs); err != nil || bs < 1024 {
		t.Fatalf("block_size = %d: %v", bs, err)
	}
	return bs
}

func TestStatSpillDB_BaselineIsTheStatementRolesWorkMem(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	app := spillRole(t, pool, "tuning03_app")
	plain := spillRole(t, pool, "tuning03_plain")
	both := spillRole(t, pool, "tuning03_both")
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	execAll(t, pool, "ALTER ROLE tuning03_app SET work_mem = '64MB'",
		"ALTER ROLE tuning03_both SET work_mem = '8MB'",
		fmt.Sprintf(`ALTER ROLE tuning03_both IN DATABASE %q SET work_mem = '24MB'`, db))
	tu := New(pool, TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
	s, err := tu.readSpillSettings(ctx, []int64{app, plain, both})
	if err != nil {
		t.Fatalf("read spill settings: %v", err)
	}
	session := settingKB(t, pool, "SELECT current_setting('work_mem')")
	reset := settingKB(t, pool, "SELECT reset_val || 'kB' FROM pg_settings WHERE name = 'work_mem'")
	if s.WorkMemKB[app] != 64*1024 || session == 64*1024 {
		t.Fatalf("app role baseline = %d kB (pg_sage session %d kB), want 65536",
			s.WorkMemKB[app], session)
	}
	if s.WorkMemKB[both] != 24*1024 {
		t.Fatalf("role-in-database beats role: %d kB, want 24576", s.WorkMemKB[both])
	}
	if s.WorkMemKB[plain] != reset {
		t.Fatalf("no role setting: %d kB, want pg_settings.reset_val %d kB",
			s.WorkMemKB[plain], reset)
	}
	blockSize := serverBlockSize(t, pool)
	if s.BlockSize != blockSize {
		t.Fatalf("block_size = %d, want the server's %d", s.BlockSize, blockSize)
	}
	// 16 MB per call by the 64 MB role gives no hint although pg_sage's own
	// session has 4 MB.
	tu.observeSpills(ctx, []candidate{spillCand(77, app, 10, 0)})
	c := spillCand(77, app, 1_010, 1_000*16*1024*1024/blockSize)
	tu.observeSpills(ctx, []candidate{c})
	if got := CombineHints(tu.prescribeAll(tu.gatherSymptoms(ctx, c))); got != "" {
		t.Fatalf("hint = %q: the app role's 64 MB work_mem holds 16 MB per call", got)
	}
}

func TestStatSpillDB_DatabaseSettingBeatsTheServerDefault(t *testing.T) {
	// A database of its own: a database-level setting would leak into the
	// package fixture's later sessions (their reset_val).
	pool, ctx := requireTunerDB(t)
	plain := spillRole(t, pool, "tuning03_dbonly")
	other, err := pgxpool.ParseConfig(testdb.CreateDatabase(t, "tuning03_dbset"))
	if err != nil {
		t.Fatalf("parse extra database DSN: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %q SET work_mem = '12MB'`,
		other.ConnConfig.Database)); err != nil {
		t.Fatalf("alter database: %v", err)
	}
	dbPool, err := pgxpool.NewWithConfig(ctx, other)
	if err != nil {
		t.Fatalf("connect extra database: %v", err)
	}
	t.Cleanup(dbPool.Close)
	tu := New(dbPool, TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
	s, err := tu.readSpillSettings(ctx, []int64{plain})
	if err != nil || s.WorkMemKB[plain] != 12*1024 {
		t.Fatalf("database setting: %d kB, %v; want 12288", s.WorkMemKB[plain], err)
	}
}

func TestStatSpillDB_ReadErrors(t *testing.T) {
	pool, _ := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tu.readSpillSettings(ctx, []int64{10}); err == nil ||
		!strings.Contains(err.Error(), "work_mem") {
		t.Fatalf("cancelled read: %v, want an error naming what was read", err)
	}
	none := New(nil, TunerConfig{}, nil, noopLogFn)
	if _, err := none.readSpillSettings(context.Background(), []int64{10}); err == nil {
		t.Fatal("no pool: want an error")
	}
}

// spillRoleName runs the spilling statement: its role-level work_mem of
// 64 kB is the statement's baseline, so a sort of a few MB spills and the
// test stays fast.
const spillRoleName = "tuning03_spill"

// spillQuery sorts about 2 MB of rows: it spills at 64 kB.
const spillQuery = `SELECT g, md5(g::text) AS h FROM generate_series(1, 20000) g ORDER BY h`

func runSpill(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+spillRoleName); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET work_mem = '64kB'"); err != nil {
		t.Fatalf("set work_mem: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := conn.Exec(ctx, spillQuery); err != nil {
			t.Fatalf("spill query: %v", err)
		}
	}
	if _, err := conn.Exec(ctx, "RESET ROLE; RESET work_mem"); err != nil {
		t.Fatalf("reset session: %v", err)
	}
}

type spillCounters struct{ queryID, userID, calls, blocks int64 }

func readSpillCounters(t *testing.T, pool *pgxpool.Pool, userID int64) spillCounters {
	t.Helper()
	var c spillCounters
	err := pool.QueryRow(context.Background(), `SELECT queryid, userid::int8, calls,
		temp_blks_written FROM pg_stat_statements
		WHERE query LIKE 'SELECT g, md5(g::text) AS h%' AND userid = $1::oid
		  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		userID).Scan(&c.queryID, &c.userID, &c.calls, &c.blocks)
	if err != nil {
		t.Fatalf("pg_stat_statements row of the spill query: %v", err)
	}
	return c
}

func statSpillFinding(t *testing.T, tu *Tuner, queryID int64) (string, bool) {
	t.Helper()
	findings, err := tu.Tune(context.Background(), nil)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	for _, f := range findings {
		if f.ObjectIdentifier != fmt.Sprintf("queryid:%d", queryID) {
			continue
		}
		syms, _ := f.Detail["symptoms"].([]string)
		for _, s := range syms {
			if s == string(SymptomStatTempSpill) {
				hint, _ := f.Detail["hint_directive"].(string)
				return hint, true
			}
		}
	}
	return "", false
}

// resetStatement resets one statement's pg_stat_statements counters only,
// leaving other packages' statements on the shared server alone.
func resetStatement(t *testing.T, pool *pgxpool.Pool, c spillCounters) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `SELECT pg_stat_statements_reset($1::oid,
		(SELECT oid FROM pg_database WHERE datname = current_database()), $2)`,
		c.userID, c.queryID); err != nil {
		t.Fatalf("reset the statement's counters: %v", err)
	}
}

func TestStatSpillDB_RealSpillIsSizedPerCall(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	role := spillRole(t, pool, spillRoleName)
	execAll(t, pool, "ALTER ROLE "+spillRoleName+" SET work_mem = '64kB'")
	runSpill(t, pool, 1)
	first := readSpillCounters(t, pool, role)
	if first.blocks == 0 {
		t.Fatalf("the spill query wrote no temp blocks: %+v", first)
	}
	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = $1", first.queryID)
	}
	clean()
	t.Cleanup(clean)
	tu := New(pool, TunerConfig{WorkMemMaxMB: 512, MinQueryCalls: 1, PlanTimeRatio: 1e9},
		nil, noopLogFn)
	runSpill(t, pool, 49)
	if _, found := statSpillFinding(t, tu, first.queryID); found {
		t.Fatal("CHECK-01: no stat_temp_spill finding on the first cycle after startup")
	}
	resetStatement(t, pool, first)
	runSpill(t, pool, 5)
	if _, found := statSpillFinding(t, tu, first.queryID); found {
		t.Fatal("CHECK-01: no stat_temp_spill finding on the first cycle after a reset")
	}
	before := readSpillCounters(t, pool, role)
	runSpill(t, pool, 50)
	after := readSpillCounters(t, pool, role)
	hint, found := statSpillFinding(t, tu, first.queryID)
	if !found {
		t.Fatalf("no stat_temp_spill finding after 50 spilling calls (%+v -> %+v)",
			before, after)
	}
	calls := after.calls - before.calls
	perCallKB := ceilDiv((after.blocks-before.blocks)*serverBlockSize(t, pool), calls*1024)
	_, wantMB, ok := sizeStatSpillWorkMem(perCallKB, 64, 512)
	if !ok || hint != fmtSetWorkMem(wantMB) || wantMB >= 512 || calls != 50 {
		t.Fatalf("hint = %q, want %s from %d kB per call over %d calls", hint,
			fmtSetWorkMem(wantMB), perCallKB, calls)
	}
}

func TestStatSpillDB_PlanHintIsCappedByHostMemory(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	const qid int64 = 880_303
	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = $1", qid)
	}
	clean()
	t.Cleanup(clean)
	plan := `[{"Plan": {"Node Type": "Sort", "Sort Method": "external merge",
		"Sort Space Used": 1048576, "Sort Space Type": "Disk", "Plan Rows": 500000,
		"Plans": []}}]`
	if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (queryid, plan_json, source)
		VALUES ($1, $2::jsonb, 'test')`, qid, plan); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	tu := New(pool, TunerConfig{WorkMemMaxMB: 1024}, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true}, noopLogFn)
	tu.WithHostMemory(hostOf(4 << 30))
	fs := tu.processCandidate(ctx, candidate{QueryID: qid, Query: "SELECT * FROM t ORDER BY x",
		Calls: 10})
	if len(fs) != 1 {
		t.Fatalf("findings = %+v", fs)
	}
	f := fs[0]
	if f.Detail["hint_directive"] != `Set(work_mem "204MB")` ||
		!strings.Contains(f.RecommendedSQL, `Set(work_mem "204MB")`) {
		t.Fatalf("CHECK-02: plan hint %v / %q, want the 204MB host cap", f.Detail["hint_directive"],
			f.RecommendedSQL)
	}
	b, _ := f.Detail["work_mem_budget"].(map[string]any)
	if b["requested_mb"] != 1024 || b["ceiling_mb"] != 204 || b["clamped"] != true {
		t.Fatalf("budget detail = %v", f.Detail["work_mem_budget"])
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT hint_text FROM sage.query_hints WHERE queryid = $1",
		qid).Scan(&stored); err != nil || stored != `Set(work_mem "204MB")` {
		t.Fatalf("sage.query_hints hint = %q, %v; want the clamped hint", stored, err)
	}
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
