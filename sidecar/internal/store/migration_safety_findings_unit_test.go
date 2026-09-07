package store

import "testing"

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
