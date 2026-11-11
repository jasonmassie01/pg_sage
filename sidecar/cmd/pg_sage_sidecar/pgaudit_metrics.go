package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/pgaudit"
)

// writePGAuditMetrics emits what became of each database's pgaudit records
// (audit-06): stored, dropped (lost to failures or caps), evicted (pushed
// out of the log subscription unread), requeued (in a failed batch, sent
// again) and unmatched (no principal or action of pg_sage's to keep it for).
func writePGAuditMetrics(b *strings.Builder, counts []pgaudit.RecordCount) {
	metricsreg.Header(b, "pg_sage_pgaudit_records_total")
	for _, c := range counts {
		writeSample(b, "pg_sage_pgaudit_records_total",
			withLabel(dbLabel(c.Database), "outcome", c.Outcome), c.Count)
	}
	b.WriteString("\n")
}
