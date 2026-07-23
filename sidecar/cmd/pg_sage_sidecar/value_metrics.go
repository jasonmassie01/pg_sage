package main

import (
	"context"
	"fmt"
	"strings"
)

// writeValueMetrics emits the DBA-hours-saved value series: verified
// toil minutes saved by database and feature, and incidents avoided by
// kind. It reads the same tables as /api/v1/value, so only
// verified-successful actions are counted and reverted actions
// contribute nothing.
func writeValueMetrics(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_toil_minutes_saved_total " +
		"Verified DBA toil minutes saved\n" +
		"# TYPE pg_sage_toil_minutes_saved_total counter\n")
	rows, err := pool.Query(ctx, `SELECT COALESCE(d.name, ''),
		al.action_type, sum(al.toil_minutes_saved)::float8
		FROM sage.action_log al
		LEFT JOIN sage.databases d ON d.id = al.database_id
		WHERE al.outcome = 'success' AND al.toil_minutes_saved IS NOT NULL
		GROUP BY 1, 2`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var database, feature string
			var minutes float64
			if rows.Scan(&database, &feature, &minutes) == nil {
				fmt.Fprintf(b, "pg_sage_toil_minutes_saved_total"+
					"{database=%q,feature=%q} %g\n",
					database, feature, minutes)
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_incidents_avoided_total " +
		"Incidents avoided by kind\n" +
		"# TYPE pg_sage_incidents_avoided_total counter\n")
	kindRows, err := pool.Query(ctx, `SELECT kind, count(*)::bigint
		FROM sage.incident_avoided GROUP BY kind`)
	if err == nil {
		defer kindRows.Close()
		for kindRows.Next() {
			var kind string
			var count int64
			if kindRows.Scan(&kind, &count) == nil {
				fmt.Fprintf(b, "pg_sage_incidents_avoided_total"+
					"{kind=%q} %d\n", kind, count)
			}
		}
	}
	b.WriteString("\n")
}
