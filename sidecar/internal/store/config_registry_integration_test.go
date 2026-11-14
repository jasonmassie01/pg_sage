//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// config-02b, error propagation on real PostgreSQL: a row stored before a
// range moved is skipped on replay with a warning naming the key and its
// updated_at, and the next save of the key replaces it.
func TestStoredOverrideOutOfTheNewRangeIsSkippedAndTheNextSaveWorks(t *testing.T) {
	pool := setupConfigTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const key = "analyzer.self_cost_budget_ms"
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, database_id, updated_at)
		VALUES ($1, '5000000', NULL, now())`, key); err != nil {
		t.Fatalf("seed the pre-config-02b row: %v", err)
	}
	cs := NewConfigStore(pool)
	row := storedOverride(ctx, t, cs, key)
	if _, ok, problem := ReplayOverride(row.Key, row.Value); ok {
		t.Fatal("the out-of-range row replayed")
	} else if msg := StoredOverrideWarning(row, problem); !strings.Contains(msg, key) ||
		!strings.Contains(msg, "updated_at 20") {
		t.Fatalf("warning %q must name the key and updated_at", msg)
	}
	if err := cs.SetOverride(ctx, key, "4000", 0, 1); err != nil {
		t.Fatalf("the next save failed: %v", err)
	}
	if err := cs.SetOverride(ctx, key, "3600001", 0, 1); err == nil ||
		!strings.Contains(err.Error(), key) {
		t.Fatalf("an out-of-range save = %v, want a refusal naming the key", err)
	}
	if row = storedOverride(ctx, t, cs, key); row.Value != "4000" {
		t.Fatalf("after the save the row holds %q, want 4000", row.Value)
	}
	if _, ok, _ := ReplayOverride(row.Key, row.Value); !ok {
		t.Fatal("the saved row does not replay")
	}
}

// storedOverride returns the one global row of key (sage.config also holds
// runtime state such as emergency_stop).
func storedOverride(ctx context.Context, t *testing.T, cs *ConfigStore, key string) ConfigOverride {
	t.Helper()
	rows, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	var found []ConfigOverride
	for _, r := range rows {
		if r.Key == key {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rows for %s = %+v, want exactly one", key, found)
	}
	return found[0]
}
