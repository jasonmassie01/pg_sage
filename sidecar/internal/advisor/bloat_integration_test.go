//go:build integration

package advisor

import (
	"context"
	"testing"
)

func TestBloatEstimate_LivePG_DeadTuples(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT schemaname, relname, n_dead_tup, n_live_tup,
		        pg_table_size(schemaname || '.' || relname) as table_bytes
		 FROM pg_stat_user_tables
		 WHERE n_dead_tup > 100 AND n_live_tup > 0
		 ORDER BY n_dead_tup DESC LIMIT 5`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var schema, rel string
		var dead, live, tableBytes int64
		if err := rows.Scan(&schema, &rel, &dead, &live, &tableBytes); err != nil {
			t.Fatal(err)
		}
		total := dead + live
		ratio := float64(dead) / float64(total) * 100
		sizeMB := float64(tableBytes) / (1024 * 1024)
		t.Logf("  %s.%s: dead=%d live=%d ratio=%.1f%% size=%.1fMB",
			schema, rel, dead, live, ratio, sizeMB)
		count++
	}
	if count == 0 {
		t.Skip("no tables with significant dead tuples")
	}
}

func TestBloatEstimate_LivePG_Extensions(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT name FROM pg_available_extensions
		 WHERE name IN ('pg_repack', 'pgstattuple', 'pg_buffercache')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Logf("  available: %s", name)
	}
}

func TestBloatEstimate_LivePG_TableSizes(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT relname, pg_table_size(oid), pg_indexes_size(oid)
		 FROM pg_class
		 WHERE relkind = 'r' AND relnamespace = 'public'::regnamespace
		 ORDER BY pg_table_size(oid) DESC LIMIT 5`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var tableSize, indexSize int64
		if err := rows.Scan(&name, &tableSize, &indexSize); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %s: table=%.1fMB indexes=%.1fMB",
			name, float64(tableSize)/(1024*1024), float64(indexSize)/(1024*1024))
	}
}
