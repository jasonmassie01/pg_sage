package tuning

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: the tuning store's fresh EXPLAIN and the rehearsal's plan
// cost plan the application's statement on the application's search_path,
// on a session pinned to pg_catalog, pg_temp.
// No concurrent access tests: the path is transaction-local (set_config
// is_local) on the caller's own session.

func serverVersionNum(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestExplainAfresh_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "tuning_apppath")
	explain := PlanExplainer(pinned, serverVersionNum(t, pinned), catalogread.Default())
	for _, q := range []string{"SELECT * FROM orders", `SELECT qty FROM "Order Items"`} {
		plan, err := explain(context.Background(), q)
		if err != nil {
			t.Fatalf("explain %q: %v", q, err)
		}
		if plan.Source != PlanSourceExplain || !strings.Contains(string(plan.JSON), "Node Type") {
			t.Fatalf("explain %q: source %q plan %s", q, plan.Source, plan.JSON)
		}
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the EXPLAIN = %q, want %q", got, testdb.PinnedPath)
	}
}

func TestExplainAfresh_MissingTableAndDecoyFail(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "tuning_missing")
	explain := PlanExplainer(pinned, serverVersionNum(t, pinned), catalogread.Default())
	for q, want := range map[string]string{
		"SELECT * FROM missing_orders": `relation "missing_orders" does not exist`,
		"SELECT sage_only FROM orders": `column "sage_only" does not exist`,
	} {
		plan, err := explain(context.Background(), q)
		if err == nil || !strings.Contains(err.Error(), want) || plan.JSON != nil {
			t.Fatalf("explain %q: err = %v plan = %s, want %q", q, err, plan.JSON, want)
		}
	}
}

func TestExplainAfresh_ServerDefaultPathLeavesOutUser(t *testing.T) {
	pinned := testdb.DefaultPathDatabase(t, "tuning_default")
	explain := PlanExplainer(pinned, serverVersionNum(t, pinned), catalogread.Default())
	if _, err := explain(context.Background(), "SELECT public_only FROM orders"); err != nil {
		t.Fatalf("explain on public: %v", err)
	}
	_, err := explain(context.Background(), "SELECT user_only FROM orders")
	if err == nil || !strings.Contains(err.Error(), `column "user_only" does not exist`) {
		t.Fatalf("err = %v, want the $user schema left out", err)
	}
}

func TestPlanCost_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "tuning_cost")
	version := serverVersionNum(t, pinned)
	cost, err := planCostOn(t, pinned, "SELECT app_only FROM orders", version)
	if err != nil || cost <= 0 {
		t.Fatalf("cost = %v err = %v, want a positive cost", cost, err)
	}
	_, err = planCostOn(t, pinned, "SELECT * FROM missing_orders", version)
	if err == nil || !strings.Contains(err.Error(), `relation "missing_orders" does not exist`) {
		t.Fatalf("err = %v, want the does-not-exist error", err)
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the plan = %q, want %q", got, testdb.PinnedPath)
	}
}

// planCostOn runs planCost on one of pool's sessions and releases it, so
// a failing test never leaves the one-session pool's session acquired.
func planCostOn(t *testing.T, pool *pgxpool.Pool, text string, version int) (float64, error) {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	return planCost(context.Background(), conn.Conn(), text, version)
}
