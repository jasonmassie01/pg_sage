package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrency test: each rendering is one read-only transaction on its
// own pooled connection, and the drop it feeds runs under the DDL lease.

const (
	spRbTrgm    = "public.sp_rb_trgm"
	spRbPartial = "public.sp_rb_partial"
)

// setupPinnedRollbackIndexes builds two indexes whose application-path
// definitions name objects outside pg_catalog without a schema: a pg_trgm
// operator class and an enum type in a partial-index predicate. It returns
// those definitions as the collector captures them.
func setupPinnedRollbackIndexes(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) (trgmDef, partialDef string) {
	t.Helper()
	ensurePgTrgmInPublic(t, ctx, pool)
	for _, sql := range []string{
		"DROP TABLE IF EXISTS public.sp_rb_t",
		"DROP TYPE IF EXISTS public.sp_rb_state",
		"CREATE TYPE public.sp_rb_state AS ENUM ('open', 'closed')",
		"CREATE TABLE public.sp_rb_t (id int, email text, state public.sp_rb_state)",
		"CREATE INDEX sp_rb_trgm ON public.sp_rb_t USING gin (email gin_trgm_ops)",
		"CREATE INDEX sp_rb_partial ON public.sp_rb_t (id) WHERE state = 'open'",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DROP TABLE IF EXISTS public.sp_rb_t")
		_, _ = pool.Exec(cctx, "DROP TYPE IF EXISTS public.sp_rb_state")
	})
	for _, c := range []struct {
		index string
		def   *string
	}{{spRbTrgm, &trgmDef}, {spRbPartial, &partialDef}} {
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.pg_get_indexdef($1::regclass)",
			c.index).Scan(c.def); err != nil {
			t.Fatalf("capture %s: %v", c.index, err)
		}
	}
	// Guard against a vacuous test: the captured text really is unqualified.
	if !strings.Contains(trgmDef, "(email gin_trgm_ops)") ||
		!strings.Contains(partialDef, "'open'::sp_rb_state") {
		t.Fatalf("captured definitions are already qualified: %q / %q", trgmDef, partialDef)
	}
	return trgmDef, partialDef
}

func ensurePgTrgmInPublic(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var schema string
	err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT n.nspname FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_trgm'), '')`).Scan(&schema)
	if err != nil {
		t.Fatalf("check pg_trgm: %v", err)
	}
	switch schema {
	case "public":
		return
	case "":
		if _, err := pool.Exec(ctx, "CREATE EXTENSION pg_trgm SCHEMA public"); err != nil {
			t.Fatalf("create pg_trgm in public (contrib is in every CI image): %v", err)
		}
	default:
		t.Fatalf("pg_trgm is installed in %s; this test needs it in public", schema)
	}
}

func liveIndexDef(t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(pg_catalog.pg_get_indexdef(
		pg_catalog.to_regclass($1)), '')`, index).Scan(&def); err != nil {
		t.Fatalf("pg_get_indexdef %s: %v", index, err)
	}
	return def
}

// Happy path and nil/empty: the pinned rendering qualifies every object
// outside pg_catalog; a missing index renders as "" without an error.
func TestSearchPathPinRendersQualifiedIndexDefinition(t *testing.T) {
	pool, ctx := requireDB(t)
	setupPinnedRollbackIndexes(t, ctx, pool)
	trgm, err := pinnedIndexDefinition(ctx, pool, spRbTrgm)
	if err != nil || !strings.Contains(trgm, "(email public.gin_trgm_ops)") {
		t.Fatalf("pinned trigram definition = %q, %v; want public.gin_trgm_ops", trgm, err)
	}
	partial, err := pinnedIndexDefinition(ctx, pool, spRbPartial)
	if err != nil || !strings.Contains(partial, "'open'::public.sp_rb_state") {
		t.Fatalf("pinned partial definition = %q, %v; want public.sp_rb_state", partial, err)
	}
	missing, err := pinnedIndexDefinition(ctx, pool, "public.sp_rb_no_such_index")
	if err != nil || missing != "" {
		t.Fatalf("missing index rendered %q, %v; want empty and no error", missing, err)
	}
	// Invalid input: an unqualified name does not resolve through public.
	bare, err := pinnedIndexDefinition(ctx, pool, "sp_rb_trgm")
	if err != nil || bare != "" {
		t.Fatalf("unqualified index rendered %q, %v; want empty and no error", bare, err)
	}
}

// Error propagation: a cancelled context surfaces as an error, never as an
// empty definition the caller would read as "index gone".
func TestSearchPathPinRenderPropagatesError(t *testing.T) {
	pool, _ := requireDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	def, err := pinnedIndexDefinition(ctx, pool, spRbTrgm)
	if err == nil || def != "" {
		t.Fatalf("cancelled render = %q, %v; want an error", def, err)
	}
	if _, err := (&Executor{pool: pool}).pinDropRollback(ctx,
		"DROP INDEX CONCURRENTLY "+spRbTrgm, "CREATE INDEX x ON public.t (a);"); err == nil {
		t.Fatal("pinDropRollback swallowed the render error")
	}
}

// State transition (drop, then rollback under the pin): the re-rendered
// rollback recreates the same index. The captured application-path text
// with an enum in its predicate cannot run in the pinned session (types are
// never resolved); the one with a bare extension operator class now runs,
// because the executor names the extension's schema (qualifyIndexOpclasses).
func TestSearchPathPinDropRollbackRecreatesIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	trgmDef, partialDef := setupPinnedRollbackIndexes(t, ctx, pool)
	e := &Executor{pool: pool}
	cfg := RollbackMonitorConfig{StatementTimeout: time.Minute, LockTimeoutMs: 5000}
	for _, c := range []struct {
		index, captured string
		capturedRuns    bool
	}{
		{spRbTrgm, trgmDef, true}, {spRbPartial, partialDef, false},
	} {
		drop := "DROP INDEX CONCURRENTLY " + c.index
		rollback, err := e.pinDropRollback(ctx, drop, c.captured+";")
		if err != nil {
			t.Fatalf("pinDropRollback %s: %v", c.index, err)
		}
		want := liveIndexDef(t, ctx, pool, c.index)
		if err := ExecConcurrently(ctx, pool, drop, time.Minute); err != nil {
			t.Fatalf("drop %s: %v", c.index, err)
		}
		_, err = executeRollbackSQL(ctx, pool, c.captured+";", cfg)
		switch {
		case c.capturedRuns && err != nil:
			t.Fatalf("captured rollback %q under the pin: %v", c.captured, err)
		case !c.capturedRuns && err == nil:
			t.Fatalf("captured rollback %q ran under the pin; the test is vacuous", c.captured)
		case c.capturedRuns:
			if got := liveIndexDef(t, ctx, pool, c.index); got != want {
				t.Fatalf("captured rollback recreated %s = %q, want %q", c.index, got, want)
			}
			if err := ExecConcurrently(ctx, pool, drop, time.Minute); err != nil {
				t.Fatalf("drop %s again: %v", c.index, err)
			}
		}
		if _, err := executeRollbackSQL(ctx, pool, rollback, cfg); err != nil {
			t.Fatalf("pinned rollback %q: %v", rollback, err)
		}
		if got := liveIndexDef(t, ctx, pool, c.index); got != want {
			t.Fatalf("recreated %s = %q, want %q", c.index, got, want)
		}
	}
}

// Boundary: only a DROP INDEX with a CREATE INDEX rollback is re-rendered;
// everything else passes through unchanged and issues no query.
func TestSearchPathPinDropRollbackLeavesOtherStatements(t *testing.T) {
	e := &Executor{} // a nil pool panics if any case reaches the database
	cases := []struct{ sql, rollback string }{
		{"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY public.i"},
		{"DROP INDEX CONCURRENTLY public.i", ""},
		{"DROP INDEX CONCURRENTLY public.i", "-- recreate from version control"},
		{"VACUUM public.t", "CREATE INDEX i ON public.t (a);"},
		{"", ""},
	}
	for _, c := range cases {
		got, err := e.pinDropRollback(context.Background(), c.sql, c.rollback)
		if err != nil || got != c.rollback {
			t.Errorf("pinDropRollback(%q, %q) = %q, %v; want it unchanged",
				c.sql, c.rollback, got, err)
		}
	}
}

// Integration through the operator action: the ledger records the pinned
// rollback, so a revert of an approved drop recreates the trigram index.
func TestSearchPathPinManualDropRecordsPinnedRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = SetEmergencyStop(ctx, pool, false, "test")
	trgmDef, _ := setupPinnedRollbackIndexes(t, ctx, pool)
	want := liveIndexDef(t, ctx, pool, spRbTrgm)
	drop := "DROP INDEX CONCURRENTLY " + spRbTrgm
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql, rollback_sql)
		 VALUES ('test_sp_rollback', 'warning', 'index', $1,
		         'drop trigram index', '{}', 'rec', $2, $3)
		 RETURNING id`, spRbTrgm, drop, trgmDef+";").Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = pool.Exec(cctx, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	e := pinnedRollbackExecutor(pool)
	if _, err := e.ExecuteManual(ctx, findingID, drop, trgmDef+";", nil); err != nil {
		t.Fatalf("ExecuteManual drop: %v", err)
	}
	if got := liveIndexDef(t, ctx, pool, spRbTrgm); got != "" {
		t.Fatalf("index still present after the drop: %q", got)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(rollback_sql, '') FROM sage.action_log
		WHERE finding_id = $1 ORDER BY id DESC LIMIT 1`, findingID).Scan(&stored); err != nil {
		t.Fatalf("read action_log: %v", err)
	}
	if !strings.Contains(stored, "public.gin_trgm_ops") {
		t.Fatalf("recorded rollback %q is not the pinned rendering", stored)
	}
	cfg := RollbackMonitorConfig{StatementTimeout: time.Minute, LockTimeoutMs: 5000}
	if _, err := executeRollbackSQL(ctx, pool, stored, cfg); err != nil {
		t.Fatalf("revert the drop with %q: %v", stored, err)
	}
	if got := liveIndexDef(t, ctx, pool, spRbTrgm); got != want {
		t.Fatalf("reverted index = %q, want %q", got, want)
	}
}

// Integration through the autonomous path: the finding's rollback is
// replaced before the drop runs, so the action records the pinned text.
func TestSearchPathPinAutonomousDropUsesPinnedRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	_, partialDef := setupPinnedRollbackIndexes(t, ctx, pool)
	f := analyzer.Finding{RecommendedSQL: "DROP INDEX CONCURRENTLY " + spRbPartial,
		RollbackSQL: partialDef + ";"}
	e := pinnedRollbackExecutor(pool)
	if _, err := e.prepareAndRunFinding(ctx, &f, map[string]any{},
		ActionPolicyDecision{}); err != nil {
		t.Fatalf("prepareAndRunFinding: %v", err)
	}
	if !strings.Contains(f.RollbackSQL, "'open'::public.sp_rb_state") {
		t.Fatalf("finding rollback %q is not the pinned rendering", f.RollbackSQL)
	}
	if got := liveIndexDef(t, ctx, pool, spRbPartial); got != "" {
		t.Fatalf("index still present after the drop: %q", got)
	}
}

// The restored-outside check compares in the pinned rendering too: an
// index recreated from a pinned rollback is the same index, not a conflict.
func TestSearchPathPinRestoredCheckMatchesPinnedRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	setupPinnedRollbackIndexes(t, ctx, pool)
	rollback, err := pinnedIndexDefinition(ctx, pool, spRbTrgm)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	state, _, err := rollbackIndexTarget(ctx, pool, rollback+";")
	if err != nil || state != rollbackTargetSame {
		t.Fatalf("rollbackIndexTarget = %d, %v; want rollbackTargetSame", state, err)
	}
}

// Index replace reads the old index as its undo re-creates it, so a
// recreate text that cannot resolve under the pin is caught before the drop.
func TestSearchPathPinReplaceReadsPinnedOldDefinition(t *testing.T) {
	pool, ctx := requireDB(t)
	setupPinnedRollbackIndexes(t, ctx, pool)
	e := &Executor{pool: pool}
	old, err := e.readOldIndex(ctx, IndexReplace{OldIndex: spRbTrgm, Table: "public.sp_rb_t"})
	if err != nil {
		t.Fatalf("readOldIndex: %v", err)
	}
	if !strings.Contains(old.Definition, "public.gin_trgm_ops") || !old.OnTable {
		t.Fatalf("old index = %+v; want the pinned definition on public.sp_rb_t", old)
	}
}

func pinnedRollbackExecutor(pool *pgxpool.Pool) *Executor {
	cfg := &config.Config{Safety: config.SafetyConfig{
		DDLTimeoutSeconds: 30, LockTimeoutMs: 5000}}
	cfg.Trust.Level = "advisory"
	e := &Executor{pool: pool, cfg: cfg, recentActions: make(map[string]time.Time),
		logFn: func(string, string, ...any) {}, execMode: "auto"}
	withTestStandingGate(e)
	return e
}
