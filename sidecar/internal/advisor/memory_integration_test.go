//go:build integration

package advisor

import (
	"context"
	"testing"
)

func TestMemContext_LivePG_Settings(t *testing.T) {
	pool := testPool(t)
	for _, name := range []string{
		"shared_buffers", "work_mem",
		"effective_cache_size", "maintenance_work_mem",
	} {
		var setting, unit string
		err := pool.QueryRow(context.Background(),
			"SELECT setting, COALESCE(unit,'') FROM pg_settings WHERE name = $1",
			name).Scan(&setting, &unit)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		t.Logf("  %s = %s%s", name, setting, unit)
	}
}

func TestMemContext_LivePG_CacheHitRatio(t *testing.T) {
	pool := testPool(t)
	var hit, read int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(shared_blks_hit),0),
		        COALESCE(sum(shared_blks_read),0)
		 FROM pg_stat_statements`).Scan(&hit, &read)
	if err != nil {
		t.Skipf("pg_stat_statements: %v", err)
	}
	total := hit + read
	if total > 0 {
		ratio := float64(hit) / float64(total) * 100
		t.Logf("cache hit ratio: %.2f%% (hit=%d, read=%d)", ratio, hit, read)
	} else {
		t.Log("no block stats yet (fresh instance)")
	}
}

func TestMemContext_LivePG_TempSpills(t *testing.T) {
	pool := testPool(t)
	var spillCount int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_stat_statements WHERE temp_blks_written > 0").Scan(&spillCount)
	if err != nil {
		t.Skipf("pg_stat_statements: %v", err)
	}
	t.Logf("queries with temp spills: %d", spillCount)
}
