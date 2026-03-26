//go:build integration

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestVacuumContext_LivePG_TablesExist(t *testing.T) {
	pool := testPool(t)
	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_stat_user_tables WHERE schemaname = 'public'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count == 0 {
		t.Skip("no user tables found — load test data first")
	}
	t.Logf("found %d user tables", count)
}

func TestVacuumContext_LivePG_DeadTuples(t *testing.T) {
	pool := testPool(t)
	var tableName string
	var deadTup int64
	err := pool.QueryRow(context.Background(),
		"SELECT relname, n_dead_tup FROM pg_stat_user_tables WHERE n_dead_tup > 0 ORDER BY n_dead_tup DESC LIMIT 1").Scan(&tableName, &deadTup)
	if err != nil {
		t.Skip("no tables with dead tuples found")
	}
	t.Logf("table with most dead tuples: %s (%d)", tableName, deadTup)
	if deadTup <= 0 {
		t.Error("expected dead tuples > 0")
	}
}

func TestVacuumContext_LivePG_ConfigSnapshot(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		"SELECT name, setting FROM pg_settings WHERE name LIKE 'autovacuum%' LIMIT 5")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %s = %s", name, setting)
		count++
	}
	if count == 0 {
		t.Error("expected autovacuum settings")
	}
}

func TestVacuumContext_LivePG_Reloptions(t *testing.T) {
	pool := testPool(t)
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.reloptions IS NOT NULL AND n.nspname = 'public'`).Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("tables with custom reloptions: %d", count)
}

func TestVacuumContext_LivePG_DetectPlatform(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		"SELECT name, source FROM pg_settings WHERE name = 'max_connections'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var settings []collector.PGSetting
	for rows.Next() {
		var s collector.PGSetting
		if err := rows.Scan(&s.Name, &s.Source); err != nil {
			t.Fatal(err)
		}
		settings = append(settings, s)
	}
	platform := detectPlatform(settings)
	t.Logf("detected platform: %s", platform)
	if platform == "" {
		t.Error("expected non-empty platform")
	}
}
