package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b CHECK-28 on real PostgreSQL: through the executor's standing
// gate, the real fact store and the catalog resolver, a confirmed
// do_not_touch fact blocks an operator-approved action on its table (named
// in the decision), while the rollback of pg_sage's own index on the same
// table stays allowed. Expiring the fact lifts the block.
// No concurrent access tests here: the binder's snapshot read is covered
// in internal/facts/do_not_touch_test.go.

// doNotTouchFixture creates a table with an index and returns their names.
func doNotTouchFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string,
	string) {
	t.Helper()
	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	table, index := "orders_dnt_"+suffix, "idx_dnt_"+suffix
	for _, sql := range []string{"CREATE TABLE public." + table + " (id int, status text)",
		"CREATE INDEX " + index + " ON public." + table + " (status)"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS public."+table)
		_, _ = pool.Exec(bg, "DELETE FROM sage.facts WHERE subject = $1", "public."+table)
	})
	return table, index
}

func TestDoNotTouchThroughTheExecutorGate(t *testing.T) {
	pool, ctx := requireDB(t)
	table, index := doNotTouchFixture(t, ctx, pool)
	store := facts.NewStore(pool)
	fact, err := store.Declare(ctx, facts.Proposal{Type: facts.TypeDoNotTouch,
		Kind: facts.KindTable, Subject: "public." + table}, "alice@example.com", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	exec := factBoundExecutor(true, facts.NewBinder(store, facts.NewCatalogResolver(pool), nil))

	reindex := "REINDEX INDEX CONCURRENTLY public." + index
	got := exec.explainOperatorAction(ctx, reindex)
	if got.Decision != PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonBoundByFact) ||
		!strings.Contains(got.Detail, fmt.Sprintf("fact #%d", fact.ID)) ||
		!strings.Contains(got.Detail, "must not be touched") ||
		!strings.Contains(got.Detail, "confirmed by alice@example.com") ||
		!strings.Contains(got.Detail, "route: hold") {
		t.Fatalf("operator reindex on a do-not-touch table: %+v", got)
	}
	if _, err := exec.authorizeOperatorAction(ctx, reindex, 0, nil); err == nil ||
		!strings.Contains(err.Error(), "bound by fact") {
		t.Fatalf("operator authorization: err = %v, want bound by fact", err)
	}
	rollback := exec.explainRollback(ctx, 0, "DROP INDEX CONCURRENTLY public."+index)
	if rollback.BlockedReason == string(policy.ReasonBoundByFact) ||
		rollback.BlockedReason == string(policy.ReasonFactsUnavailable) ||
		rollback.Decision != PolicyDecisionExecute {
		t.Fatalf("rollback of pg_sage's own index: %+v, want execute", rollback)
	}
	if _, err := store.Expire(ctx, fact.ID, "bob@example.com", "change agreed"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if after := exec.explainOperatorAction(ctx, reindex); after.BlockedReason ==
		string(policy.ReasonBoundByFact) {
		t.Fatalf("expired fact still blocks: %+v", after)
	}
}
