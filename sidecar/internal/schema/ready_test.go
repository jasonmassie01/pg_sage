package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func freshPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Integration: readiness reports a missing schema, then passes once
// Bootstrap has run, then names a table dropped afterwards.
func TestCheckReady_StateTransitionsOnRealPostgres(t *testing.T) {
	pool := freshPool(t, "schema_ready")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := CheckReady(ctx, pool)
	if !errors.Is(err, ErrSchemaNotReady) || !strings.Contains(err.Error(), "findings") {
		t.Fatalf("before bootstrap: err = %v, want ErrSchemaNotReady naming tables", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := CheckReady(ctx, pool); err != nil {
		t.Fatalf("after bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE sage.briefings"); err != nil {
		t.Fatal(err)
	}
	err = CheckReady(ctx, pool)
	if !errors.Is(err, ErrSchemaNotReady) || !strings.Contains(err.Error(), "briefings") ||
		strings.Contains(err.Error(), "findings") {
		t.Fatalf("after drop: err = %v, want only briefings named", err)
	}
}

func TestCheckReady_NilPoolIsAnError(t *testing.T) {
	if err := CheckReady(context.Background(), nil); err == nil ||
		errors.Is(err, ErrSchemaNotReady) {
		t.Fatalf("nil pool: err = %v, want a distinct error", err)
	}
}

func TestCheckReady_QueryFailureIsNotReportedAsMissingTables(t *testing.T) {
	pool := freshPool(t, "schema_ready_closed")
	pool.Close()
	err := CheckReady(context.Background(), pool)
	if err == nil || errors.Is(err, ErrSchemaNotReady) {
		t.Fatalf("closed pool: err = %v, want a query error", err)
	}
}

func TestCheckReady_CancelledContextFails(t *testing.T) {
	pool := freshPool(t, "schema_ready_cancel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckReady(ctx, pool); err == nil {
		t.Fatal("cancelled context reported ready")
	}
}

func TestReadinessTables_MatchTheBootstrapList(t *testing.T) {
	names := readinessTables()
	if len(names) != len(expectedTables) {
		t.Fatalf("readiness checks %d tables, bootstrap creates %d", len(names),
			len(expectedTables))
	}
	for i, table := range expectedTables {
		if names[i] != table.name {
			t.Fatalf("table %d = %q, want %q", i, names[i], table.name)
		}
	}
}
