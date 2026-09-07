package collector

import (
	"context"
	"testing"
)

// No parallel fixtures: this verifies ordered pagination on one test database.
func TestAuditIndexPaginationAcrossSchemas(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE SCHEMA audit_indexes_a;
  CREATE SCHEMA audit_indexes_b;
  CREATE TABLE audit_indexes_a.items (id integer PRIMARY KEY, value text);
  CREATE INDEX value_idx ON audit_indexes_a.items(value);
  CREATE TABLE audit_indexes_b.items (id integer PRIMARY KEY, value text);
  CREATE INDEX value_idx ON audit_indexes_b.items(value)`)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, "DROP SCHEMA audit_indexes_a CASCADE; DROP SCHEMA audit_indexes_b CASCADE")
	for _, batch := range []int{1, 2, 0, -1} {
		cfg := testConfig()
		cfg.Collector.BatchSize = batch
		c := New(pool, cfg, 160000, noopLog)
		got, err := c.collectIndexes(ctx)
		if err != nil {
			t.Fatalf("batch %d: %v", batch, err)
		}
		seen := map[string]bool{}
		for _, idx := range got {
			if idx.SchemaName != "audit_indexes_a" && idx.SchemaName != "audit_indexes_b" {
				continue
			}
			key := idx.SchemaName + "." + idx.IndexRelName
			if seen[key] {
				t.Fatalf("duplicate paged index %s", key)
			}
			seen[key] = true
			if !idx.IsValid || idx.IndexType != "btree" || idx.IndexDef == "" {
				t.Fatalf("missing index metadata: %#v", idx)
			}
		}
		if len(seen) != 4 {
			t.Fatalf("batch %d returned %d fixture indexes, want 4", batch, len(seen))
		}
	}
}

func TestAuditIndexCatalogQueryIsBounded(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	c := New(pool, cfg, 160000, noopLog)
	rows, err := c.catalogQuery(context.Background(), indexStatsSQL, "", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count > 1 {
		t.Fatalf("bounded query returned %d rows, want at most one", count)
	}
}
