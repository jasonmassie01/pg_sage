package tuner

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-04 integration on real PostgreSQL. The test server preloads
// pg_hint_plan with enable_hint_table at its default (off). Settings are
// made with ALTER DATABASE / ALTER ROLE ... IN DATABASE on a scratch
// database only, so no other test database sees them.

// readinessDB creates a scratch database with pg_hint_plan installed and
// returns a pool on it plus its name. It skips without pg_hint_plan 1.7.
func readinessDB(t *testing.T, label string) (*pgxpool.Pool, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, label)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if exists, hasQueryID := hintTableLayout(t, pool); !exists || !hasQueryID {
		t.Skip("pg_hint_plan 1.7 hint table not installed on this server")
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return pool, name, ctx
}

// scratchRole creates a login role dropped at cleanup; its settings live
// only in the scratch database.
func scratchRole(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	ident := pgx.Identifier{name}.Sanitize()
	execAll(t, pool, "DROP ROLE IF EXISTS "+ident, "CREATE ROLE "+ident+" LOGIN")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+ident)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident)
	})
	return ident
}

// (a) Preloaded with the default enable_hint_table=off: no application
// effect, and the finding is advisory with the reason. This failed before
// tuning-04 (Available was true).
func TestDetectHintPlan_DB_DefaultOffIsNotReady(t *testing.T) {
	pool, _, ctx := readinessDB(t, "tuning04_off")
	got, err := DetectHintPlan(ctx, pool)
	if err != nil {
		t.Fatalf("DetectHintPlan: %v", err)
	}
	if !got.SharedPreload || !got.Preload || got.HintTableOn || got.ApplicationEffect ||
		!got.HintTableReady || !got.Privilege || got.Method != "shared_preload" {
		t.Fatalf("default-off facts = %+v", got)
	}
	if !reflect.DeepEqual(got.Reasons, []string{reasonHintTableOff}) {
		t.Fatalf("reasons = %q, want [%q]", got.Reasons, reasonHintTableOff)
	}
	// CHECK-05: query_tuning findings carry hint_readiness and no SQL.
	tu := New(pool, TunerConfig{}, got, noopLogFn)
	if tu.HintsAvailable() {
		t.Fatal("HintsAvailable with enable_hint_table off")
	}
	f := tu.buildFinding(ctx, candidate{QueryID: 9_240_401, Query: "SELECT 1"}, nil,
		readinessHint, "t", "r")
	if f.RecommendedSQL != "" || f.Detail["hint_readiness"] != reasonHintTableOff {
		t.Fatalf("finding sql=%q hint_readiness=%v", f.RecommendedSQL,
			f.Detail["hint_readiness"])
	}
}

// (b) A role-level setting in the scratch database gives application
// effect; the same setting for the whole database does too.
func TestDetectHintPlan_DB_RoleAndDatabaseSettings(t *testing.T) {
	pool, db, ctx := readinessDB(t, "tuning04_role")
	role := scratchRole(t, pool, "tuning04_app")
	dbIdent := pgx.Identifier{db}.Sanitize()
	execAll(t, pool, "ALTER ROLE "+role+" IN DATABASE "+dbIdent+
		" SET pg_hint_plan.enable_hint_table = on")
	got, err := ProbeHintReadiness(ctx, pool)
	if err != nil || !got.ApplicationEffect || !got.HintTableOn || len(got.Reasons) != 0 {
		t.Fatalf("role-level on: %+v, %v", got, err)
	}
	execAll(t, pool, "ALTER ROLE "+role+" IN DATABASE "+dbIdent+
		" RESET pg_hint_plan.enable_hint_table")
	if got, _ := ProbeHintReadiness(ctx, pool); got.ApplicationEffect {
		t.Fatalf("after RESET: %+v", got)
	}
	execAll(t, pool, "ALTER DATABASE "+dbIdent+" SET pg_hint_plan.enable_hint_table = on")
	if got, err := ProbeHintReadiness(ctx, pool); err != nil || !got.ApplicationEffect {
		t.Fatalf("database-level on: %+v, %v", got, err)
	}
}

// (c) A role without INSERT on hint_plan.hints gives false with reason
// privilege; the superuser on the same database is ready (control). A role
// without pg_read_all_settings cannot see shared_preload_libraries, so the
// probe cannot confirm the preload and says so (fail closed).
func TestDetectHintPlan_DB_RoleWithoutInsert(t *testing.T) {
	pool, db, ctx := readinessDB(t, "tuning04_priv")
	role := scratchRole(t, pool, "tuning04_reader")
	execAll(t, pool,
		"ALTER DATABASE "+pgx.Identifier{db}.Sanitize()+
			" SET pg_hint_plan.enable_hint_table = on",
		"GRANT USAGE ON SCHEMA hint_plan TO "+role,
		"GRANT SELECT, DELETE ON hint_plan.hints TO "+role)
	if got, err := ProbeHintReadiness(ctx, pool); err != nil || !got.ApplicationEffect {
		t.Fatalf("superuser control: %+v, %v", got, err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
	steps := []struct {
		grant   string
		preload bool
		reasons []string
	}{
		{"", false, []string{reasonPreloadUnreadable, reasonPrivilege}},
		{"GRANT pg_read_all_settings TO " + role, true, []string{reasonPrivilege}},
	}
	for _, s := range steps {
		if s.grant != "" {
			execAll(t, pool, s.grant)
		}
		got, err := ProbeHintReadiness(ctx, conn)
		if err != nil {
			t.Fatalf("probe as %s: %v", role, err)
		}
		if got.ApplicationEffect || got.Privilege || !got.HintTableReady ||
			!got.HintTableOn || got.Preload != s.preload {
			t.Fatalf("role without INSERT (%q): %+v", s.grant, got)
		}
		if !reflect.DeepEqual(got.Reasons, s.reasons) {
			t.Fatalf("%q: reasons = %q, want %q", s.grant, got.Reasons, s.reasons)
		}
	}
}

// State transition on real PostgreSQL: a running tuner picks up
// ALTER DATABASE ... SET enable_hint_table = on at its next cycle.
func TestTune_DB_PicksUpEnableHintTableWithoutRestart(t *testing.T) {
	pool, db, ctx := readinessDB(t, "tuning04_cycle")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	detected, err := DetectHintPlan(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	tu := New(pool, TunerConfig{MinQueryCalls: 1, PlanTimeRatio: 0.5}, detected, noopLogFn)
	if _, err := tu.Tune(ctx, nil); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if tu.HintsAvailable() {
		t.Fatal("hints available before the setting was turned on")
	}
	execAll(t, pool, fmt.Sprintf("ALTER DATABASE %s SET pg_hint_plan.enable_hint_table = on",
		pgx.Identifier{db}.Sanitize()))
	if _, err := tu.Tune(ctx, nil); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if !tu.HintsAvailable() {
		t.Fatalf("setting not picked up by the next cycle: %+v", tu.hints())
	}
	f := tu.buildFinding(ctx, candidate{QueryID: 9_240_402, Query: "SELECT 2"}, nil,
		readinessHint, "t", "r")
	if !strings.Contains(f.RecommendedSQL, "INSERT INTO hint_plan.hints") {
		t.Fatalf("after the setting: SQL %q", f.RecommendedSQL)
	}
}

// PG14-16 ship pg_hint_plan 1.4-1.6, whose hint_plan.hints has no query_id:
// the probe reads that layout without error and reports it, even with the
// table setting on for the database.
func TestDetectHintPlan_DB_PreQueryIDLayoutIsNotReady(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "tuning04_layout"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if exists, hasQueryID := hintTableLayout(t, pool); !exists || hasQueryID {
		t.Skip("needs the pre-1.7 hint table (pg_hint_plan 1.4-1.6, PG14-16)")
	}
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	execAll(t, pool, "ALTER DATABASE "+pgx.Identifier{db}.Sanitize()+
		" SET pg_hint_plan.enable_hint_table = on")
	got, err := DetectHintPlan(ctx, pool)
	if err != nil {
		t.Fatalf("DetectHintPlan: %v", err)
	}
	if got.ApplicationEffect || got.HintTableReady || !got.HintTableOn || !got.Preload {
		t.Fatalf("pre-1.7 layout facts = %+v", got)
	}
	if !reflect.DeepEqual(got.Reasons, []string{reasonLayout}) {
		t.Fatalf("reasons = %q, want [%q]", got.Reasons, reasonLayout)
	}
}

// pg_hint_plan.enable_hint off for the database disables every hint, so the
// table setting alone is not application effect.
func TestDetectHintPlan_DB_EnableHintOffIsNotReady(t *testing.T) {
	pool, db, ctx := readinessDB(t, "tuning04_hintoff")
	dbIdent := pgx.Identifier{db}.Sanitize()
	execAll(t, pool,
		"ALTER DATABASE "+dbIdent+" SET pg_hint_plan.enable_hint_table = on",
		"ALTER DATABASE "+dbIdent+" SET pg_hint_plan.enable_hint = off")
	got, err := ProbeHintReadiness(ctx, pool)
	if err != nil || got.ApplicationEffect || got.HintTableOn {
		t.Fatalf("enable_hint off: %+v, %v", got, err)
	}
	if !reflect.DeepEqual(got.Reasons, []string{reasonHintsOff}) {
		t.Fatalf("reasons = %q, want [%q]", got.Reasons, reasonHintsOff)
	}
	execAll(t, pool, "ALTER DATABASE "+dbIdent+" RESET pg_hint_plan.enable_hint")
	if got, err := ProbeHintReadiness(ctx, pool); err != nil || !got.ApplicationEffect {
		t.Fatalf("after RESET enable_hint: %+v, %v", got, err)
	}
}
