package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/migration"
)

func TestNewMigrationSafetyFindingStoreKeepsPool(t *testing.T) {
	got := NewMigrationSafetyFindingStore(nil)

	if got == nil {
		t.Fatal("NewMigrationSafetyFindingStore returned nil")
	}
	if got.pool != nil {
		t.Fatalf("pool = %#v, want nil", got.pool)
	}
}

func TestNullableImpact(t *testing.T) {
	if got := nullableImpact(0); got != nil {
		t.Fatalf("nullableImpact(0) = %#v, want nil", got)
	}
	if got := nullableImpact(0.7); got != 0.7 {
		t.Fatalf("nullableImpact(0.7) = %#v, want 0.7", got)
	}
}

// Nil/empty: a store without a pool fails fast with an actionable error
// instead of dereferencing nil.
func TestMigrationSafetyUpsertWithoutPoolFails(t *testing.T) {
	id, err := NewMigrationSafetyFindingStore(nil).UpsertMigrationSafetyFinding(
		context.Background(), migration.MigrationSafetyFinding{ObjectIdentifier: "x"})
	if err == nil || id != 0 {
		t.Fatalf("id=%d err=%v, want 0 and an error", id, err)
	}
	if !strings.Contains(err.Error(), "no database pool") {
		t.Fatalf("error %q does not say the pool is missing", err)
	}
}

// Invalid input: a detail that cannot be encoded is rejected before any
// SQL runs.
func TestMigrationSafetyUpsertRejectsUnencodableDetail(t *testing.T) {
	store := &MigrationSafetyFindingStore{pool: new(pgxpool.Pool)}
	id, err := store.UpsertMigrationSafetyFinding(context.Background(),
		migration.MigrationSafetyFinding{Detail: map[string]any{"c": make(chan int)}})
	if err == nil || id != 0 || !strings.Contains(err.Error(), "encoding migration") {
		t.Fatalf("id=%d err=%v, want an encoding error", id, err)
	}
}
