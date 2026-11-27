package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/backtestfx"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// Live: while each one-shot command runs, its sessions are pg_sage's own
// (application_name pg_sage, doctor's pg_sage_doctor; left out by
// ActivityExclusionSQL; search_path pinned), and every statement it sends
// is recorded in pg_stat_statements with the tag right after its first
// keyword, its pins among them. Each test has fresh databases and reads
// pg_stat_statements for them only (dbid), after resetting their entries.
// Ledger and verifier-backtest run on a database whose path puts planted
// public operators first. Concurrent access: two ledger runs at once.

// cliPoolSession is a session pg_stat_activity shows waiting on a lock.
type cliPoolSession struct {
	App, Query string
	Kept       bool // ActivityExclusionSQL is true: read as not pg_sage's
}

// cliPoolBlockedSQL lists the current database's sessions waiting on a
// lock of relation $1.
var cliPoolBlockedSQL = `SELECT a.application_name, a.query, ` +
	selfmonitor.ActivityExclusionSQL("a") + `
	FROM pg_catalog.pg_stat_activity a
	JOIN pg_catalog.pg_locks l ON l.pid = a.pid AND NOT l.granted
	WHERE l.relation = $1::regclass AND a.datname = pg_catalog.current_database()
	ORDER BY a.pid`

// cliPoolObserve holds ACCESS EXCLUSIVE on table through admin while run
// runs in a goroutine, and returns the want sessions of admin's database
// waiting on it; the lock is released and run has finished when it
// returns.
func cliPoolObserve(t *testing.T, admin *pgxpool.Pool, table string, want int,
	run func()) []cliPoolSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	seen, err := cliPoolWaitBlocked(ctx, admin, table, want, done)
	if rbErr := tx.Rollback(context.Background()); rbErr != nil && err == nil {
		err = fmt.Errorf("release the lock on %s: %w", table, rbErr)
	}
	<-done
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

func cliPoolWaitBlocked(ctx context.Context, admin *pgxpool.Pool, table string, want int,
	done <-chan struct{}) ([]cliPoolSession, error) {
	for {
		rows, err := admin.Query(ctx, cliPoolBlockedSQL, table)
		if err != nil {
			return nil, fmt.Errorf("read the sessions waiting on %s: %w", table, err)
		}
		seen, err := pgx.CollectRows(rows, pgx.RowToStructByPos[cliPoolSession])
		if err != nil {
			return nil, fmt.Errorf("read the sessions waiting on %s: %w", table, err)
		}
		if len(seen) >= want {
			return seen, nil
		}
		select {
		case <-done:
			return nil, fmt.Errorf("the command ended before %d session(s) waited on %s "+
				"(saw %d)", want, table, len(seen))
		case <-ctx.Done():
			return nil, fmt.Errorf("%d session(s) waiting on %s, want %d: %w", len(seen),
				table, want, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// cliPoolAssertOwn requires each observed session to be named app, left
// out by ActivityExclusionSQL, and running a statement containing marker
// with the tag right after its first keyword.
func cliPoolAssertOwn(t *testing.T, seen []cliPoolSession, app, marker string) {
	t.Helper()
	for _, s := range seen {
		if s.App != app || s.Kept {
			t.Errorf("running session application_name %q, kept by ActivityExclusionSQL "+
				"%v; want %q, left out", s.App, s.Kept, app)
		}
		if !cliPoolTagged(s.Query) || !strings.Contains(s.Query, marker) {
			t.Errorf("running statement %q: want the tag right after its first keyword "+
				"of a statement containing %q", s.Query, marker)
		}
	}
}

// cliPoolOwnSQL is what a session shows of itself: application_name,
// search_path, default_transaction_isolation, default_transaction_read_only
// and whether ActivityExclusionSQL keeps its pg_stat_activity row.
var cliPoolOwnSQL = `SELECT pg_catalog.current_setting('application_name'),
	pg_catalog.current_setting('search_path'),
	pg_catalog.current_setting('default_transaction_isolation'),
	pg_catalog.current_setting('default_transaction_read_only'),
	(SELECT ` + selfmonitor.ActivityExclusionSQL("a") + `
	   FROM pg_catalog.pg_stat_activity a WHERE a.pid = pg_catalog.pg_backend_pid())`

// cliPoolQuerier is a pool or a transaction.
type cliPoolQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// cliPoolAssertOwnSession requires q's session to be named app, pinned and
// left out by ActivityExclusionSQL, and returns its read-only default.
func cliPoolAssertOwnSession(t *testing.T, q cliPoolQuerier, app string) string {
	t.Helper()
	var gotApp, path, iso, readOnly string
	var kept bool
	if err := q.QueryRow(context.Background(), cliPoolOwnSQL).Scan(&gotApp, &path, &iso,
		&readOnly, &kept); err != nil {
		t.Fatalf("read the session's own settings: %v", err)
	}
	if gotApp != app || path != testdb.PinnedPath || iso != "read committed" || kept {
		t.Fatalf("session application_name %q search_path %q default isolation %q, kept "+
			"by ActivityExclusionSQL %v; want %q, %q, read committed, left out", gotApp,
			path, iso, kept, app, testdb.PinnedPath)
	}
	return readOnly
}

// cliPoolDB is a database whose pg_stat_statements entries a check reads,
// with the markers its recorded statements must contain.
type cliPoolDB struct {
	admin   *pgxpool.Pool
	markers []string
}

// cliPoolStatementsSQL is every top-level statement text recorded for the
// current database except this read (its alias) and the reset.
const cliPoolStatementsSQL = `SELECT s.query AS cli_pool_reader FROM %s s
	WHERE s.toplevel AND s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
	  WHERE d.datname = pg_catalog.current_database())
	  AND strpos(s.query, 'cli_pool_reader') = 0
	  AND strpos(s.query, 'pg_stat_statements_reset') = 0`

// cliPoolPgss resets each database's pg_stat_statements entries, runs the
// command, then requires every statement recorded since in each database
// to carry the tag right after its first keyword and each of its markers
// to be among them. A reset or eviction by another package during an
// attempt repeats it (pgssepoch).
func cliPoolPgss(t *testing.T, run func(), dbs ...cliPoolDB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rels := make([]string, len(dbs))
	for i, db := range dbs {
		rel, err := extschema.StatementsRel(ctx, db.admin, "pg_stat_statements")
		if err != nil {
			t.Fatalf("find pg_stat_statements: %v", err)
		}
		rels[i] = rel
	}
	pgssepoch.Attempt(t, ctx, dbs[0].admin, 3, func() []string {
		for _, db := range dbs {
			if _, err := testdb.ResetStatements(ctx, db.admin); err != nil {
				t.Fatalf("reset pg_stat_statements: %v", err)
			}
		}
		run()
		var problems []string
		for i, db := range dbs {
			rows, err := db.admin.Query(ctx, fmt.Sprintf(cliPoolStatementsSQL, rels[i]))
			if err != nil {
				t.Fatalf("read pg_stat_statements: %v", err)
			}
			texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read pg_stat_statements: %v", err)
			}
			problems = append(problems, cliPoolProblems(texts, db.markers)...)
		}
		return problems
	})
}

// cliPoolWaitGone waits until no pg_sage session is left in admin's
// database: a session reports its statistics as it ends, before its
// pg_stat_activity entry goes.
func cliPoolWaitGone(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	sql := `SELECT count(*) FROM pg_catalog.pg_stat_activity a
		WHERE a.datname = pg_catalog.current_database()
		  AND NOT (` + selfmonitor.ActivityExclusionSQL("a") + `)`
	for deadline := time.Now().Add(30 * time.Second); ; {
		var n int
		if err := admin.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatalf("count pg_sage sessions: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d pg_sage sessions still open 30s after the command ended", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cliPoolPlantReachable runs read on a session that keeps the planted
// database's own path and requires it to call planted_texteq: the
// command's statements resolve = through the path, so the pinned runs
// calling no plant is the pin's doing.
func cliPoolPlantReachable(t *testing.T, plant *testdb.PlantedPath,
	read func(context.Context, *pgxpool.Pool) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, plant.DSN)
	if err != nil {
		t.Fatalf("unpinned pool: %v", err)
	}
	defer pool.Close()
	if err := read(ctx, pool); err != nil {
		t.Fatalf("the command's read on the database's own path: %v", err)
	}
	if err := testdb.FlushIdleSessions(ctx, pool); err != nil {
		t.Fatalf("flush the unpinned session: %v", err)
	}
	var calls int64
	if err := plant.Admin.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0)::int8
		FROM pg_catalog.pg_stat_user_functions
		WHERE schemaname = 'public' AND funcname = 'planted_texteq'`).Scan(&calls); err != nil {
		t.Fatalf("read planted_texteq calls: %v", err)
	}
	if calls <= 1 {
		t.Fatalf("planted_texteq calls = %d after the unpinned read, want more than the "+
			"control's 1: the pinned runs' zero would prove nothing", calls)
	}
}

func cliPoolAdmin(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ledger: two concurrent runs wait on sage.action_log as pg_sage, left out
// by ActivityExclusionSQL, sending the tagged ledger read, and both print
// the change; a run's recorded statements are all tagged, pins included;
// no run calls a planted object, which the same read on the database's own
// path does.
func TestCLIPoolDB_LedgerSessionsArePgSagesOwn(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "clipool_ledger")
	if err := schema.Bootstrap(context.Background(), plant.Admin); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	seedChange(t, plant.Admin, "orders", "create_index", "success", "improved",
		time.Now().Add(-time.Hour))
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": plant.DSN}
	args := []string{"--since", "24h", "--database", "orders"}
	codes, outs := make([]int, 2), make([]string, 2)
	seen := cliPoolObserve(t, plant.Admin, "sage.action_log", 2, func() {
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() { codes[i], outs[i], _ = runLedgerCLI(t, env, args...) })
		}
		wg.Wait()
	})
	cliPoolAssertOwn(t, seen, selfmonitor.ApplicationName, "FROM sage.action_log al")
	for i := range 2 {
		if codes[i] != 0 || !strings.Contains(outs[i], "Add an index") {
			t.Fatalf("concurrent run %d: exit %d stdout %q, want 0 and the change", i,
				codes[i], outs[i])
		}
	}
	cliPoolPgss(t, func() {
		if code, out, errOut := runLedgerCLI(t, env, args...); code != 0 {
			t.Fatalf("ledger: exit %d stdout %q stderr %q", code, out, errOut)
		}
	}, cliPoolDB{plant.Admin, cliPoolMarkers(
		"SELECT " + selfmonitor.StatementTag + " al.id, al.executed_at")})
	cliPoolWaitGone(t, plant.Admin)
	plant.AssertNoPlantedCalls(t, plant.Admin)
	cliPoolPlantReachable(t, plant, func(ctx context.Context, pool *pgxpool.Pool) error {
		res, err := outcomes.Read(ctx, pool, "orders", outcomes.Query{Since: 24 * time.Hour,
			Now: time.Now(), Limit: outcomes.DefaultLimit})
		if err == nil && len(res.Rows) != 1 {
			err = fmt.Errorf("%d rows, want the seeded 1", len(res.Rows))
		}
		return err
	})
}

// verifier-backtest with its default open (sagepool.New), on 30 days of
// history: the pool is pg_sage's own session, a full run waits on
// sage.snapshots as pg_sage and stores its result, every statement it
// records is tagged, and no run calls a planted object.
func TestCLIPoolDB_BacktestDefaultOpenIsPgSagesOwn(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "clipool_backtest")
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, plant.Admin); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Hour).Add(-31 * 24 * time.Hour)
	now := start.Add(30 * 24 * time.Hour)
	m, err := backtest.FixtureAt(backtest.FixtureNoisyNeighbour, start, 30, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backtestfx.Persist(t, plant.Admin, fxSource(m), 15*time.Minute, 35)
	pool, err := defaultBacktestDeps().open(ctx, plant.DSN)
	if err != nil {
		t.Fatalf("default open: %v", err)
	}
	cliPoolAssertOwnSession(t, pool, selfmonitor.ApplicationName)
	pool.Close()
	deps := cliPoolBacktestDeps(t, plant.DSN, now)
	run := func() {
		var out, errOut bytes.Buffer
		code := runVerifierBacktest(ctx, []string{"--database", "fixture", "--placebos", "24",
			"--seed", "3"}, deps, &out, &errOut)
		if code != 0 || !strings.Contains(out.String(), "verifier back-test of fixture") {
			t.Errorf("verifier-backtest: exit %d stdout %q stderr %q", code, out.String(),
				errOut.String())
		}
	}
	seen := cliPoolObserve(t, plant.Admin, "sage.snapshots", 1, run)
	cliPoolAssertOwn(t, seen, selfmonitor.ApplicationName, "sage.snapshots")
	tag := selfmonitor.StatementTag
	cliPoolPgss(t, run, cliPoolDB{plant.Admin, cliPoolMarkers(
		"SELECT "+tag+" pg_try_advisory_lock(",
		"SELECT "+tag+" min(s.collected_at), max(s.collected_at)",
		"INSERT "+tag+" INTO sage.verifier_backtest")})
	cliPoolWaitGone(t, plant.Admin)
	plant.AssertNoPlantedCalls(t, plant.Admin)
	cliPoolPlantReachable(t, plant, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, _, ok, err := backtest.NewPostgresHistory(pool).Span(ctx)
		if err == nil && !ok {
			err = errors.New("no history span")
		}
		return err
	})
}

// cliPoolBacktestDeps is the command's default open over a configuration
// that registers dsn's database as "fixture" the way a fleet entry does
// (sslmode prefer), at a fixed now.
func cliPoolBacktestDeps(t *testing.T, dsn string, now time.Time) backtestDeps {
	t.Helper()
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Databases = []config.DatabaseConfig{{Name: "fixture", Host: parsed.Host,
		Port: int(parsed.Port), User: parsed.User, Password: parsed.Password,
		Database: parsed.Database}}
	deps := defaultBacktestDeps()
	deps.load = func(string) (*config.Config, error) { return config.Clone(cfg), nil }
	deps.now = func() time.Time { return now }
	return deps
}

// publish: the snapshot openPublishSource opens is pg_sage's own session
// and keeps it read-only; every statement it sends is recorded tagged,
// its BEGIN and its bounds among them (the extension normalizes the
// bounds' literals, so the marker stops at set_config).
func TestCLIPoolDB_PublishSourceIsPgSagesOwn(t *testing.T) {
	admin, dsn, name := publishDB(t)
	seedChange(t, admin, name, "create_index", "success", "improved",
		time.Now().Add(-time.Hour))
	ctx := context.Background()
	read := func() {
		src, err := openPublishSource(ctx, dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer src.Close()
		cliPoolAssertOwnSession(t, src.(*pgPublishSource).tx, selfmonitor.ApplicationName)
		res, err := src.ReadOutcomes(ctx, outcomes.Query{Since: 24 * time.Hour,
			Now: time.Now(), Limit: outcomes.DefaultLimit})
		if err != nil || len(res.Rows) != 1 || res.Rows[0].Verdict != "improved" {
			t.Fatalf("outcomes = %+v, %v; want the one improved change", res.Rows, err)
		}
		if _, found, err := src.LatestBacktest(ctx); err != nil || found {
			t.Fatalf("latest back-test found %v, %v; want none stored", found, err)
		}
	}
	read()
	tag := selfmonitor.StatementTag
	cliPoolPgss(t, read, cliPoolDB{admin, cliPoolMarkers(
		"begin "+tag+" isolation level repeatable read read only",
		"SELECT "+tag+" set_config(",
		"SELECT "+tag+" al.id, al.executed_at",
		"SELECT "+tag+" ran_at, verifier, placebos")})
}

// bench export-replay: the export waits on sage.sre_investigations as
// pg_sage, left out by ActivityExclusionSQL, then exports the contest;
// every statement it records is tagged, its scope lookup and pins among
// them.
func TestCLIPoolDB_ExportReplayIsPgSagesOwn(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "clipool_export")
	id := contestedInvestigation(t, dsn)
	admin := cliPoolAdmin(t, dsn)
	export := func() {
		exp, err := exportReplayCase(context.Background(), dsn, id, false)
		if err != nil || exp.Contest.Verdict != "refuted" ||
			exp.Contest.ActualNode != "ddl_lock_queue" || exp.Case.Gold.Root != "ddl_lock_queue" {
			t.Errorf("export = %+v gold %+v, %v; want the refuted ddl_lock_queue contest",
				exp.Contest, exp.Case.Gold, err)
		}
	}
	seen := cliPoolObserve(t, admin, "sage.sre_investigations", 1, export)
	cliPoolAssertOwn(t, seen, selfmonitor.ApplicationName, "sage.sre_investigations")
	cliPoolPgss(t, export, cliPoolDB{admin, cliPoolMarkers(
		"SELECT " + selfmonitor.StatementTag + " deployment_id::text, database_id::text")})
}

// history migrate: the monitored session waits on sage.snapshots as
// pg_sage, left out by ActivityExclusionSQL, and the rows reach the store;
// every statement recorded in both databases is tagged, pins included.
func TestCLIPoolDB_HistoryMigrateIsPgSagesOwn(t *testing.T) {
	monDSN, metaDSN, mon, meta := historyDBs(t)
	seedHistoryRows(t, mon, 3)
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": monDSN, "SAGE_META_DB": metaDSN}
	migrate := func() {
		code, out, errOut := runHistoryCLI(t, env, "migrate", "--database-id", "7")
		if code != 0 || !strings.Contains(out, "sage.snapshots: copied") {
			t.Errorf("migrate: exit %d stdout %q stderr %q", code, out, errOut)
		}
	}
	seen := cliPoolObserve(t, mon, "sage.snapshots", 1, migrate)
	cliPoolAssertOwn(t, seen, selfmonitor.ApplicationName, "sage.snapshots")
	if n := count(t, meta, `SELECT count(*) FROM sage.snapshots
		WHERE database_id = 7`); n != 3 {
		t.Fatalf("the store holds %d snapshots of database 7, want the 3 migrated", n)
	}
	cliPoolPgss(t, migrate, cliPoolDB{mon, cliPoolMarkers("sage.snapshots")},
		cliPoolDB{meta, cliPoolMarkers("sage.snapshots")})
}

// doctor: its one session is named pg_sage_doctor (its parameters are set
// after the pins), pinned, read-only and left out by ActivityExclusionSQL;
// a full run's recorded statements are all tagged, pins included.
func TestCLIPoolDB_DoctorSessionIsPgSagesOwn(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "clipool_doctor")
	pool, _, err := openDoctorPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	readOnly := cliPoolAssertOwnSession(t, pool, "pg_sage_doctor")
	pool.Close()
	if readOnly != "on" {
		t.Fatalf("default_transaction_read_only = %q, want on", readOnly)
	}
	cliPoolPgss(t, func() {
		if code, out, errOut := runDoctorForTest(t, "--pg-url", dsn, "--json"); code != 0 {
			t.Fatalf("doctor: exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
	}, cliPoolDB{cliPoolAdmin(t, dsn), cliPoolMarkers()})
}
