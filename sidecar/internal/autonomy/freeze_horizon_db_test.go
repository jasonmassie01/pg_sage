package autonomy

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-08 on real PostgreSQL: the custodian judges each table against
// its effective freeze maximum (the reloption when lower), answers a
// TOAST horizon on the table that owns it, and plans (never runs) a
// catalog VACUUM.

func horizonFixture(t *testing.T, pool *pgxpool.Pool, table, with string) {
	t.Helper()
	bloatTestExec(t, pool, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id int, body text) WITH (autovacuum_enabled = false"+with+")")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	bloatTestExec(t, pool, "INSERT INTO "+table+" VALUES (1, 'x')")
	bloatTestExec(t, pool, "VACUUM (FREEZE) "+table)
}

func freezeSQLFor(proposals []Proposal, table string) []string {
	var out []string
	for _, p := range proposals {
		if p.SQL != "" && len(p.TargetObjects) == 1 && p.TargetObjects[0] == table {
			out = append(out, p.SQL)
		}
	}
	return out
}

func TestFreezeHorizonReloptionTableIsRedAndPlainTableGreen(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	tuned, plain := "public.vacuum08_tuned", "public.vacuum08_plain"
	horizonFixture(t, pool, tuned, ", autovacuum_freeze_max_age = 100000")
	horizonFixture(t, pool, plain, "")
	testdb.BurnXIDs(t, ctx, pool, 80000)

	proposals, err := NewPostgresFreezeCustodian(pool, "testdb", 25).Scan(ctx)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := freezeSQLFor(proposals, tuned)
	want := freeze.VacuumFreezeSQL("public", "vacuum08_tuned")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("tuned table freezes = %q, want exactly [%q]", got, want)
	}
	if mine := proposalsFor(proposals, plain); len(mine) != 0 {
		t.Fatalf("plain table at the same age got %#v, want nothing (green)", mine)
	}
}

func TestFreezeHorizonCatalogGetsOnlyAPlan(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	user := "public.vacuum08_user"
	horizonFixture(t, pool, user, "")
	testdb.BurnXIDs(t, ctx, pool, burnForRed(t, pool))

	proposals, err := NewPostgresFreezeCustodian(pool, "testdb", holderRedBuffer).Scan(ctx)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var plans []Proposal
	for _, p := range proposals {
		for _, target := range p.TargetObjects {
			catalog := strings.HasPrefix(target, "pg_catalog.") ||
				strings.HasPrefix(target, "information_schema.") ||
				strings.HasPrefix(target, "pg_toast.")
			if catalog && p.SQL != "" {
				t.Fatalf("catalog %s got an automatic statement %q", target, p.SQL)
			}
		}
		if p.ResponseKind == string(freeze.ResponsePlanCatalogFreeze) {
			plans = append(plans, p)
		}
	}
	if len(plans) != 1 {
		t.Fatalf("catalog plans = %#v, want exactly one per scan", plans)
	}
	plan := plans[0]
	if plan.SQL != "" || plan.Feature != "freeze" || len(plan.TargetObjects) == 0 ||
		!strings.HasPrefix(plan.TargetObjects[0], "pg_catalog.") ||
		!strings.Contains(plan.Plan, "MAINTAIN") || plan.Evidence["plan_only"] != true {
		t.Fatalf("catalog plan = %#v", plan)
	}
	if got := freezeSQLFor(proposals, user); len(got) != 1 {
		t.Fatalf("red user table freezes = %q, want one", got)
	}
}

func TestFreezeHorizonTOASTHorizonFreezesItsOwner(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	owner := "public.vacuum08_toasted"
	horizonFixture(t, pool, owner, ", toast.autovacuum_enabled = false")
	bloatTestExec(t, pool, "INSERT INTO "+owner+
		" SELECT 2, string_agg(md5(g::text), '') FROM generate_series(1, 2000) g")
	bloatTestExec(t, pool, "VACUUM (FREEZE) "+owner)
	testdb.BurnXIDs(t, ctx, pool, burnForRed(t, pool))
	// The heap is frozen again, its TOAST is not: only the TOAST is red.
	bloatTestExec(t, pool, "VACUUM (FREEZE, PROCESS_TOAST false) "+owner)

	proposals, err := NewPostgresFreezeCustodian(pool, "testdb", holderRedBuffer).Scan(ctx)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := freezeSQLFor(proposals, owner)
	if len(got) != 1 || !strings.Contains(got[0], "vacuum08_toasted") {
		t.Fatalf("owner freezes = %q, want one VACUUM of %s", got, owner)
	}
	for _, p := range proposals {
		for _, target := range p.TargetObjects {
			if strings.HasPrefix(target, "pg_toast.") {
				t.Fatalf("a TOAST relation was targeted directly: %#v", p)
			}
		}
	}
}
