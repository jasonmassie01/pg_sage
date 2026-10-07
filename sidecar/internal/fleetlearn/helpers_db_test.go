package fleetlearn

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/fleetlearn"))
}

// freshDB creates a disposable database with the sage schema bootstrapped.
// It skips only when no test server is configured.
func freshDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// tenantSchema creates the same two-table application schema under the
// given table names, so look-alike tests can vary names but not shapes.
func tenantSchema(t *testing.T, pool *pgxpool.Pool, orders, customers string) {
	t.Helper()
	exec(t, pool, "CREATE TABLE "+customers+
		" (id bigint PRIMARY KEY, email text NOT NULL, created timestamptz)")
	exec(t, pool, "CREATE TABLE "+orders+" (id bigint PRIMARY KEY, customer_id bigint, "+
		"total numeric(12,2), status text, placed timestamptz)")
	exec(t, pool, "CREATE INDEX ON "+orders+" (customer_id)")
	exec(t, pool, "CREATE UNIQUE INDEX ON "+customers+" (email)")
}

// seedOutcome records one verified action on table with the given verdict.
func seedOutcome(t *testing.T, pool *pgxpool.Pool, class, table, verdict string) {
	t.Helper()
	ctx := context.Background()
	var findingID, actionID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('missing_index','warning','table',$1,'t',
		jsonb_build_object('table',$2::text),'resolved') RETURNING id`,
		table+"|seed", table).Scan(&findingID)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, outcome) VALUES ('create_index',$1,$2,'success') RETURNING id`,
		findingID, "CREATE INDEX CONCURRENTLY ON "+table+" (status)").Scan(&actionID)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.action_outcome (action_log_id,
		action_class, predicted, prediction_method, verdict, decided_at)
		VALUES ($1,$2,'{}','none',$3, now())`, actionID, class, verdict)
	if err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
}
