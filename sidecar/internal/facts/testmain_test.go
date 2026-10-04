package facts

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/facts"))
}

// livePool connects to the package's fixture database, bootstraps the
// sage schema and starts every test with an empty fact store.
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	clean := func() {
		for _, table := range []string{"facts", "slot_consumer_registry", "table_contract",
			"action_log"} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM sage."+table); err != nil {
				t.Errorf("clean sage.%s: %v", table, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

// execAll runs statements in order, failing the test on the first error.
func execAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// uniqueName is a schema or slot name unique to this test run.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s%06x", prefix, time.Now().UnixNano()&0xffffff)
}
