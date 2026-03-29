//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres" +
		"?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
	testKey      = crypto.DeriveKey("integration-test-key")
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		dsn := testDSN()
		qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}

		testPool, testPoolErr = pgxpool.NewWithConfig(qctx, poolCfg)
		if testPoolErr != nil {
			return
		}

		if err := testPool.Ping(qctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		if err := schema.Bootstrap(qctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		schema.ReleaseAdvisoryLock(qctx, testPool)

		if err := schema.EnsureDatabasesTable(qctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("ensure databases: %w", err)
			return
		}
	})

	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}
