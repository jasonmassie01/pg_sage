//go:build integration

package advisor

import (
	"context"
	"fmt"
	"testing"
)

func TestWALContext_LivePG_Settings(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		"SELECT name, setting, COALESCE(unit,'') FROM pg_settings WHERE name IN ('max_wal_size','wal_level','checkpoint_timeout','wal_compression')")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, setting, unit string
		if err := rows.Scan(&name, &setting, &unit); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %s = %s%s", name, setting, unit)
		count++
	}
	if count < 3 {
		t.Errorf("expected at least 3 WAL settings, got %d", count)
	}
}

func TestWALContext_LivePG_WALPosition(t *testing.T) {
	pool := testPool(t)
	var lsn string
	err := pool.QueryRow(context.Background(),
		"SELECT pg_current_wal_lsn()::text").Scan(&lsn)
	if err != nil {
		t.Skipf("pg_current_wal_lsn: %v (might be replica)", err)
	}
	if lsn == "" {
		t.Error("expected non-empty LSN")
	}
	t.Logf("current WAL LSN: %s", lsn)
}

func TestWALContext_LivePG_CheckpointStats(t *testing.T) {
	pool := testPool(t)
	// PG17 uses pg_stat_checkpointer; older versions use pg_stat_bgwriter
	var versionStr string
	if err := pool.QueryRow(context.Background(),
		"SHOW server_version_num").Scan(&versionStr); err != nil {
		t.Fatalf("version: %v", err)
	}
	var versionNum int
	fmt.Sscanf(versionStr, "%d", &versionNum)
	var checkpoints int64
	var err error
	if versionNum >= 170000 {
		err = pool.QueryRow(context.Background(),
			"SELECT num_timed + num_requested FROM pg_stat_checkpointer",
		).Scan(&checkpoints)
	} else {
		err = pool.QueryRow(context.Background(),
			"SELECT checkpoints_timed + checkpoints_req FROM pg_stat_bgwriter",
		).Scan(&checkpoints)
	}
	if err != nil {
		t.Fatalf("checkpoint stats: %v", err)
	}
	t.Logf("total checkpoints: %d (PG %d)", checkpoints, versionNum)
	if checkpoints < 0 {
		t.Error("expected non-negative checkpoints")
	}
}
