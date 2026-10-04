package startup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A missing pg_stat_statements is the one prerequisite pg_sage can run
// without (catalog-only first look, degraded monitoring), so it must be
// distinguishable from a refused connection or an unsupported version.
func TestRunChecksMarksMissingStatementsDegradable(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "nostatements")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop extension: %v", err)
	}
	checks, err := RunChecks(ctx, pool)
	if !errors.Is(err, ErrStatementsUnavailable) {
		t.Fatalf("missing extension err = %v, want ErrStatementsUnavailable", err)
	}
	if checks != nil {
		t.Fatalf("checks = %+v, want nil with the error", checks)
	}
}

func TestRunChecksOtherFailuresAreNotDegradable(t *testing.T) {
	_, err := RunChecks(context.Background(), nil)
	if err == nil || errors.Is(err, ErrStatementsUnavailable) {
		t.Fatalf("nil pool err = %v, want a non-degradable error", err)
	}
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunChecks(cancelled, pool); err == nil ||
		errors.Is(err, ErrStatementsUnavailable) {
		t.Fatalf("cancelled checks err = %v, want a non-degradable error", err)
	}
}
