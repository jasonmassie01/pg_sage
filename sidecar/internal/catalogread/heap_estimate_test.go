package catalogread

import (
	"strings"
	"testing"
)

// The per-cycle scans that embed the estimate must stay catalog-only
// (measured.md M3): no relation is opened or stat()ed.
// No concurrency test: HeapExpectedPagesSQL is a pure string builder.
func TestHeapExpectedPagesSQLIsCatalogOnly(t *testing.T) {
	sql := HeapExpectedPagesSQL("cls", "nsp")
	for _, forbidden := range []string{"pg_relation_size", "pg_total_relation_size",
		"pg_table_size", "pgstattuple", "c.", "n."} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("estimate references %q:\n%s", forbidden, sql)
		}
	}
	for _, want := range []string{"cls.reltuples >= 0", "st.schemaname = nsp.nspname",
		"st.tablename = cls.relname", "NOT st.inherited", "'fillfactor'", "pg_stats"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("estimate lacks %q:\n%s", want, sql)
		}
	}
}
