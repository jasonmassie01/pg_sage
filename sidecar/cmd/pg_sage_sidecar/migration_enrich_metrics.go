package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/migration"
)

// writeMigrationEnrichMetrics emits the model requests each database's
// migration enrichment queue dropped because it was full (migration-10).
// Any rise means some risky DDL was shown without its model script.
func writeMigrationEnrichMetrics(b *strings.Builder, drops []migration.EnrichDropCount) {
	b.WriteString("# HELP pg_sage_migration_enrich_dropped_total Migration model " +
		"requests dropped because the database's enrichment queue was full\n" +
		"# TYPE pg_sage_migration_enrich_dropped_total counter\n")
	for _, d := range drops {
		fmt.Fprintf(b, "pg_sage_migration_enrich_dropped_total{database=%q} %d\n",
			d.Database, d.Count)
	}
	b.WriteString("\n")
}
