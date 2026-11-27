package provideradvice

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/provideradvice"))
}

var (
	poolOnce sync.Once
	sharedDB *pgxpool.Pool
	poolErr  error
)

// testPool is the bootstrapped fixture database, emptied of this
// package's rows and fake advisor objects before and after each test.
func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		ctx := context.Background()
		sharedDB, poolErr = pgxpool.New(ctx, dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(ctx, sharedDB)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	ctx := context.Background()
	clean := func() {
		for _, stmt := range []string{
			"DELETE FROM sage.provider_recommendations",
			"DELETE FROM sage.provider_advice_sources",
			"DELETE FROM sage.findings WHERE detail ? 'pa_test'",
			"DROP VIEW IF EXISTS public.google_db_advisor_recommended_indexes",
			"DROP SCHEMA IF EXISTS pa_test CASCADE",
		} {
			if _, err := sharedDB.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return sharedDB, ctx
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
