package backtest

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/backtestfx"
)

// The back-test on a real server (PG14-18 alike): 30 days of synthetic
// snapshots written through snapstore give the same counts through
// PostgresHistory as the in-memory run, one row per completed run, none
// for a failed one, and one run per database at a time.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/verify/backtest"))
}

func backtestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, table := range []string{"snapshots", "query_rollup_hourly",
		"query_rollup_coverage", "verifier_backtest", "action_log"} {
		if _, err := pool.Exec(t.Context(), "DELETE FROM sage."+table); err != nil {
			t.Fatalf("clear sage.%s: %v", table, err)
		}
	}
	return pool
}

const dbCycle = 15 * time.Minute

// fxSource is m as backtestfx writes it.
func fxSource(m *Memory) backtestfx.Source {
	first, last, _, _ := m.Span(context.Background())
	src := backtestfx.Source{First: first, Last: last, Breaks: m.Breaks(),
		Cycles: map[int64][]backtestfx.Cycle{}}
	for _, s := range m.Stmts {
		src.Statements = append(src.Statements, backtestfx.Statement{QueryID: s.QueryID,
			Tables: s.Tables})
		for _, iv := range m.Intervals(s.QueryID) {
			src.Cycles[s.QueryID] = append(src.Cycles[s.QueryID], backtestfx.Cycle{To: iv.To,
				Calls: iv.Calls, Blocks: iv.Blocks, Rows: iv.Rows, Ms: iv.Ms})
		}
	}
	return src
}

// dbFixture is 30 days of the traffic-shift fixture ending a day ago, so
// every hour of it is closed and rolled up.
func dbFixture(t *testing.T) (*Memory, Config) {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Hour).Add(-31 * day)
	m, err := FixtureAt(FixtureTrafficShift, start, 30, dbCycle)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Database: "fixture", Days: 90, Placebos: 24, Seed: 3,
		MaxRuntime: 5 * time.Minute, Cycle: dbCycle,
		Now: func() time.Time { return start.Add(30 * day) }}
	return m, cfg
}

func TestBacktestDB_PostgresHistoryMatchesMemory(t *testing.T) {
	pool := backtestPool(t)
	m, cfg := dbFixture(t)
	backtestfx.Persist(t, pool, fxSource(m), dbCycle, 35)
	want, err := Run(t.Context(), m, cfg)
	if err != nil {
		t.Fatalf("in-memory run: %v", err)
	}
	got, err := RunAndSave(t.Context(), pool, NewPostgresHistory(pool), cfg)
	if err != nil {
		t.Fatalf("stored run: %v", err)
	}
	if got != want {
		t.Fatalf("PostgreSQL run differs from memory:\n got %+v\nwant %+v", got, want)
	}
	if want.V2.Usable() != 24 || want.V2.Injected != 24 {
		t.Fatalf("counts %+v, want 24 usable and injected", want.V2)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.verifier_backtest
		WHERE verifier = 'v2' AND placebos = $1 AND false_positives = $2
		AND detected = $3 AND seed = 3 AND database_id IS NULL`, want.V2.Placebos,
		want.V2.FalsePositives, want.V2.Detected).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("stored rows matching the run = %d, %v; want 1", rows, err)
	}
	latest, ok, err := Latest(t.Context(), pool)
	if err != nil || !ok || latest.V2 != want.V2 || latest.V1 != want.V1 ||
		!latest.RanAt.Equal(want.RanAt) || latest.Database != "fixture" {
		t.Fatalf("Latest = %+v %v %v, want %+v", latest, ok, err, want)
	}
	b, ok, err := NewReader(pool).LatestBacktest(t.Context(), "fixture")
	if err != nil || !ok || b.Passed || !b.RanAt.Equal(want.RanAt) {
		t.Fatalf("reader = %+v %v %v: 24 placebos must not pass the gate", b, ok, err)
	}
}

func TestBacktestDB_StatementsAndActions(t *testing.T) {
	pool := backtestPool(t)
	m, cfg := dbFixture(t)
	backtestfx.Persist(t, pool, fxSource(m), dbCycle, 1)
	h := NewPostgresHistory(pool)
	_, last, _, _ := m.Span(t.Context())
	stmts, err := h.Statements(t.Context(), last)
	if err != nil || len(stmts) != len(m.Stmts) {
		t.Fatalf("Statements = %d, %v; want %d", len(stmts), err, len(m.Stmts))
	}
	for i, s := range stmts {
		w := m.Stmts[i]
		if s.QueryID != w.QueryID || s.Calls != w.Calls || len(s.Tables) != 1 ||
			s.Tables[0] != w.Tables[0] {
			t.Fatalf("statement %d = %+v, want %+v", i, s, w)
		}
	}
	at := cfg.Now().Add(-10 * day)
	for _, sql := range []string{"CREATE INDEX CONCURRENTLY i1 ON public.t2 (a)",
		"ALTER SYSTEM SET work_mem = '8MB'"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO sage.action_log (executed_at,
			action_type, sql_executed, outcome) VALUES ($1, 'x', $2, 'success')`, at,
			sql); err != nil {
			t.Fatalf("insert action: %v", err)
		}
	}
	acts, err := h.Actions(t.Context(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(acts) != 2 {
		t.Fatalf("Actions = %+v, %v", acts, err)
	}
	if len(acts[0].Tables) != 1 || acts[0].Tables[0] != "public.t2" ||
		len(acts[1].Tables) != 0 {
		t.Fatalf("action tables = %v and %v, want [public.t2] and none", acts[0].Tables,
			acts[1].Tables)
	}
}

func TestBacktestDB_FailuresStoreNoRow(t *testing.T) {
	pool := backtestPool(t)
	young, _ := FixtureAt(FixtureSeasonalWeek, time.Now().UTC().Add(-4*day), 3, time.Hour)
	backtestfx.Persist(t, pool, fxSource(young), time.Hour, 5)
	_, cfg := dbFixture(t)
	cfg.Now = time.Now
	_, err := RunAndSave(t.Context(), pool, NewPostgresHistory(pool), cfg)
	if !errors.Is(err, ErrNotEnoughHistory) || !strings.Contains(err.Error(), "3 of 28 days") {
		t.Fatalf("young history: %v", err)
	}
	m, cfg := dbFixture(t)
	f := &failing{Memory: m, after: 10}
	if _, err := RunAndSave(t.Context(), pool, f, cfg); !errors.Is(err, errSnapshot) {
		t.Fatalf("failing reads: %v", err)
	}
	cfg.MaxRuntime = time.Nanosecond
	if _, err := RunAndSave(t.Context(), pool, m, cfg); !errors.Is(err, ErrRuntimeExceeded) {
		t.Fatalf("runtime: %v", err)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.verifier_backtest`).
		Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after failed runs = %d, %v; want 0", rows, err)
	}
	if _, ok, err := Latest(t.Context(), pool); ok || err != nil {
		t.Fatalf("Latest with no rows = %v %v", ok, err)
	}
}

// Two back-tests of one database at once (the CLI and the weekly run):
// the second is refused with "back-test already running".
func TestBacktestDB_OneRunPerDatabase(t *testing.T) {
	pool := backtestPool(t)
	unlock, err := Lock(t.Context(), pool, "fixture")
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	m, cfg := dbFixture(t)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = RunAndSave(context.Background(), pool, m, cfg)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrAlreadyRunning) ||
			!strings.Contains(err.Error(), "back-test already running") {
			t.Fatalf("run %d while locked: %v", i, err)
		}
	}
	other, err := Lock(t.Context(), pool, "another")
	if err != nil {
		t.Fatalf("another database's lock: %v", err)
	}
	other()
	unlock()
	if _, err := RunAndSave(t.Context(), pool, m, cfg); err != nil {
		t.Fatalf("run after unlock: %v", err)
	}
}
