package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/pgaudit"
)

// writePGAuditMetrics emits what became of each database's pgaudit records
// (audit-06): stored, dropped (lost to failures or caps), evicted (pushed
// out of the log subscription unread), requeued (in a failed batch, sent
// again) and unmatched (no principal or action of pg_sage's to keep it for).
func writePGAuditMetrics(b *strings.Builder, counts []pgaudit.RecordCount) {
	b.WriteString("# HELP pg_sage_pgaudit_records_total pgaudit records pg_sage read, " +
		"by outcome (stored, dropped, evicted, requeued, unmatched)\n" +
		"# TYPE pg_sage_pgaudit_records_total counter\n")
	for _, c := range counts {
		fmt.Fprintf(b, "pg_sage_pgaudit_records_total{database=%q,outcome=%q} %d\n",
			c.Database, c.Outcome, c.Count)
	}
	b.WriteString("\n")
}
