package autoexplain

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: the on-demand EXPLAIN plans the application's statement
// on the application's search_path, inside its read-only transaction, on
// a session pinned to pg_catalog, pg_temp.
// No concurrent access tests: each EXPLAIN sets the path for its own
// transaction only (set_config is_local), on its own pooled session.

func TestExplainReadOnly_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "ae_apppath")
	c := &Collector{pool: pinned}
	for _, q := range []string{
		"SELECT * FROM orders",
		"SELECT app_only FROM orders WHERE id = 1",
		`SELECT qty FROM "Order Items"`,
	} {
		plan, err := c.explainReadOnly(context.Background(), q, false)
		if err != nil {
			t.Fatalf("explain %q: %v", q, err)
		}
		if !strings.Contains(string(plan), `"Node Type"`) {
			t.Fatalf("explain %q returned no plan: %s", q, plan)
		}
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the EXPLAIN = %q, want the pin %q", got, testdb.PinnedPath)
	}
}

// The decoy sage.orders never shadows the application's orders.
func TestExplainReadOnly_SageSchemaDoesNotShadow(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "ae_shadow")
	c := &Collector{pool: pinned}
	_, err := c.explainReadOnly(context.Background(), "SELECT sage_only FROM orders", false)
	if err == nil || !strings.Contains(err.Error(), `column "sage_only" does not exist`) {
		t.Fatalf("err = %v, want sage_only missing: orders must be app.orders", err)
	}
}

func TestExplainReadOnly_MissingTableSurfacesThePostgresError(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "ae_missing")
	c := &Collector{pool: pinned}
	plan, err := c.explainReadOnly(context.Background(), "SELECT * FROM missing_orders", false)
	if err == nil || !strings.Contains(err.Error(), `relation "missing_orders" does not exist`) {
		t.Fatalf("err = %v plan = %s, want the does-not-exist error", err, plan)
	}
	if plan != nil {
		t.Fatalf("plan = %s, want none on error", plan)
	}
}

func TestExplainReadOnly_ServerDefaultPathLeavesOutUser(t *testing.T) {
	pinned := testdb.DefaultPathDatabase(t, "ae_default")
	c := &Collector{pool: pinned}
	ctx := context.Background()
	if _, err := c.explainReadOnly(ctx, "SELECT public_only FROM orders", false); err != nil {
		t.Fatalf("explain on public: %v", err)
	}
	_, err := c.explainReadOnly(ctx, "SELECT user_only FROM orders", false)
	if err == nil || !strings.Contains(err.Error(), `column "user_only" does not exist`) {
		t.Fatalf("err = %v, want the $user schema left out", err)
	}
}
