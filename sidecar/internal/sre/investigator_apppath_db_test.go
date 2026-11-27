package sre

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: the investigator's EXPLAIN prepares and plans the
// application's statement on the application's search_path, on a session
// pinned to pg_catalog, pg_temp.
// No concurrent access tests: the path is transaction-local (set_config
// is_local) on the explainer's own pooled session.

func explainOnPinned(t *testing.T, pool *pgxpool.Pool, body string) probes.Result {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	name := statementName()
	defer func() {
		_ = tx.Rollback(context.Background())
		_, _ = pool.Exec(context.Background(), "DEALLOCATE "+name)
	}()
	return runExplain(ctx, tx, name, body, probes.Result{})
}

func TestRunExplain_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "sre_apppath")
	for _, body := range []string{
		"SELECT * FROM orders",
		"SELECT app_only FROM orders WHERE id = $1",
		`SELECT qty FROM "Order Items"`,
	} {
		res := explainOnPinned(t, pinned, body)
		if res.Status != probes.StatusOK || len(res.Rows) == 0 {
			t.Fatalf("explain %q: status %s reason %s error %q", body, res.Status,
				res.Reason, res.Error)
		}
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the EXPLAIN = %q, want %q", got, testdb.PinnedPath)
	}
}

func TestRunExplain_SageSchemaDoesNotShadow(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "sre_shadow")
	res := explainOnPinned(t, pinned, "SELECT sage_only FROM orders")
	if res.Status == probes.StatusOK || !strings.Contains(res.Error, "sage_only") {
		t.Fatalf("status %s error %q, want sage_only missing", res.Status, res.Error)
	}
}

func TestRunExplain_MissingTableSurfacesThePostgresError(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "sre_missing")
	res := explainOnPinned(t, pinned, "SELECT * FROM missing_orders")
	if res.Status == probes.StatusOK || len(res.Rows) != 0 ||
		!strings.Contains(res.Error, `relation "missing_orders" does not exist`) {
		t.Fatalf("status %s rows %d error %q, want the does-not-exist error",
			res.Status, len(res.Rows), res.Error)
	}
}

func TestRunExplain_ServerDefaultPathLeavesOutUser(t *testing.T) {
	pinned := testdb.DefaultPathDatabase(t, "sre_default")
	if res := explainOnPinned(t, pinned, "SELECT public_only FROM orders"); res.Status !=
		probes.StatusOK {
		t.Fatalf("explain on public: status %s error %q", res.Status, res.Error)
	}
	res := explainOnPinned(t, pinned, "SELECT user_only FROM orders")
	if res.Status == probes.StatusOK || !strings.Contains(res.Error, "user_only") {
		t.Fatalf("status %s error %q, want the $user schema left out", res.Status, res.Error)
	}
}
