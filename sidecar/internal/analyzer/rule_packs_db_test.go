package analyzer

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// market-11 on real PostgreSQL: database A and database B each have their
// own sage.findings. Disabling unused_indexes on A resolves A's findings
// with resolution rule_disabled; B is untouched; re-enabling reopens A's
// finding on the next cycle.

const secondDatabaseName = "sage_market11_b"

// secondDatabasePool opens (creating it once) a second database on the
// test server, bootstrapped like the first.
func secondDatabasePool(t *testing.T, first *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := first.Exec(ctx, "CREATE DATABASE "+secondDatabaseName)
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "42P04") {
		t.Fatalf("create second database: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.Database = secondDatabaseName
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect second database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap second database: %v", err)
	}
	releasePoolAdvisoryLocks(ctx, pool)
	return pool
}

func cleanCategory(t *testing.T, pool *pgxpool.Pool, cat string) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.alert_log WHERE finding_id IN "+
				"(SELECT id FROM sage.findings WHERE category = $1)", cat)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.findings WHERE category = $1", cat)
	}
	clean()
	t.Cleanup(clean)
}

// runPackCycle runs the rule-pack part of one cycle and finalizes it.
func runPackCycle(t *testing.T, a *Analyzer, emitted []Finding) {
	t.Helper()
	a.eval = newCycleEval()
	a.eval.evaluated("unused_index")
	all := a.applyDisabledRules(emitted)
	if err := a.finalizeCycle(context.Background(), all, a.eval.resolvable(all)); err != nil {
		t.Fatalf("finalizeCycle: %v", err)
	}
}

func resolutionOf(t *testing.T, pool *pgxpool.Pool, ident string) (string, string) {
	t.Helper()
	var resolution, by string
	err := pool.QueryRow(context.Background(), `SELECT
		COALESCE(detail->>'resolution', ''), COALESCE(detail->>'disabled_by', '')
		FROM sage.findings WHERE category = 'unused_index'
		  AND object_identifier = $1 AND status = 'resolved'
		ORDER BY id DESC LIMIT 1`, ident).Scan(&resolution, &by)
	if err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	return resolution, by
}

func TestDisabledRules_StateTransitionsPerDatabase(t *testing.T) {
	poolA := phase2Pool(t)
	poolB := secondDatabasePool(t, poolA)
	cleanCategory(t, poolA, "unused_index")
	cleanCategory(t, poolB, "unused_index")
	a := New(poolA, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	b := New(poolB, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	f := lifecycleFinding("unused_index", "public.idx_a", "info")

	runPackCycle(t, a, []Finding{f})
	runPackCycle(t, b, []Finding{f})
	a.SetDisabledRules([]string{"unused_indexes"}, "alice@example.test")
	runPackCycle(t, a, []Finding{f})
	runPackCycle(t, b, []Finding{f})

	if n := countFindingRows(t, poolA, f.Category, f.ObjectIdentifier, "open"); n != 0 {
		t.Fatalf("A open rows after disable = %d, want 0", n)
	}
	res, by := resolutionOf(t, poolA, f.ObjectIdentifier)
	if res != RuleDisabledResolution || by != "alice@example.test" {
		t.Fatalf("A resolution = %q by %q, want rule_disabled by alice", res, by)
	}
	if n := countFindingRows(t, poolB, f.Category, f.ObjectIdentifier, "open"); n != 1 {
		t.Fatalf("B open rows = %d, want 1 (untouched)", n)
	}
	if n := countFindingRows(t, poolB, f.Category, f.ObjectIdentifier, "resolved"); n != 0 {
		t.Fatalf("B resolved rows = %d, want 0", n)
	}
	if got := a.LatestFindings(); len(got) != 0 {
		t.Fatalf("A published %d findings of a disabled rule", len(got))
	}

	a.ClearDisabledRules()
	runPackCycle(t, a, []Finding{f})
	if n := countFindingRows(t, poolA, f.Category, f.ObjectIdentifier, "open"); n != 1 {
		t.Fatalf("A open rows after re-enable = %d, want 1 (reopened)", n)
	}
}

// A second disabled cycle finds nothing open and resolves nothing twice.
func TestDisabledRules_ResolveIsIdempotent(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	cleanCategory(t, pool, "unused_index")
	f := lifecycleFinding("unused_index", "public.idx_b", "info")
	runPackCycle(t, a, []Finding{f})
	a.SetDisabledRules([]string{"unused_index"}, "bob")
	runPackCycle(t, a, []Finding{f})
	runPackCycle(t, a, []Finding{f})
	if n := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved"); n != 1 {
		t.Fatalf("resolved rows = %d, want 1", n)
	}
}

// clone_schemas is derived after the first rule-pack pass (collapse of a
// schema family). Disabled, it must still be dropped and not resolvable:
// otherwise every cycle would reopen it and then resolve it as
// rule_disabled again.
func TestDisabledRules_DerivedCategoryIsDropped(t *testing.T) {
	pool := phase2Pool(t)
	for _, disabled := range [][]string{nil, {"clone_schemas"}} {
		cfg := phase2Config()
		cfg.Analyzer.DisabledRules = disabled
		a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
		a.eval = newCycleEval()
		out := a.deriveFindings(context.Background(), tenantSnapshot(5),
			tenantFindings(5), nil)
		family, _ := splitFamilyOutput(t, out)
		resolvable := a.eval.resolvable(out)[CategoryCloneSchemas]
		if disabled == nil && (len(family) != 1 || !resolvable) {
			t.Fatalf("enabled: family findings = %d, resolvable %v; want 1, true",
				len(family), resolvable)
		}
		if disabled != nil && (len(family) != 0 || resolvable) {
			t.Fatalf("disabled: family findings = %d, resolvable %v; want 0, false",
				len(family), resolvable)
		}
	}
}
