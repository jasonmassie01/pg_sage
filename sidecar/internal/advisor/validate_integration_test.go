//go:build integration

package advisor

import (
	"context"
	"testing"
)

func TestValidate_LivePG_PlatformDetection(t *testing.T) {
	pool := testPool(t)
	var source string
	err := pool.QueryRow(context.Background(),
		"SELECT source FROM pg_settings WHERE name = 'max_connections'").Scan(&source)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if source == "" {
		t.Error("expected non-empty source")
	}
	t.Logf("max_connections source: %s", source)
}

func TestValidate_LivePG_RequiresRestart(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		"SELECT name, context FROM pg_settings WHERE name IN ('max_connections', 'shared_buffers', 'work_mem', 'wal_buffers')")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, ctx string
		if err := rows.Scan(&name, &ctx); err != nil {
			t.Fatal(err)
		}
		needsRestart := (ctx == "postmaster")
		if RequiresRestart(name) != needsRestart {
			t.Errorf("%s: RequiresRestart=%v but pg_settings context=%s",
				name, RequiresRestart(name), ctx)
		}
	}
}
