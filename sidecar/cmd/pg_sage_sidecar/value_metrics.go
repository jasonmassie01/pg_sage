package main

import (
	"context"
	"fmt"
	"strings"
)

// writeValueMetrics emits the DBA-hours-saved value series. It reads
// the same tables as /api/v1/value, so only verified-successful
// actions are counted and reverted actions contribute nothing.
//
// Net verified toil can decrease when a rollback zeroes an action's
// credit, so it is a gauge (SURF-18); a counter would make rate()
// report a reset spike. pg_sage_value_metrics_up reports whether the
// value queries succeeded on this scrape.
func writeValueMetrics(b *strings.Builder, ctx context.Context) {
	var toil, incidents strings.Builder
	errToil := writeToilSeries(&toil, ctx)
	errIncidents := writeIncidentSeries(&incidents, ctx)
	b.WriteString("# HELP pg_sage_toil_minutes_saved " +
		"Net verified DBA toil minutes saved (decreases on rollback)\n" +
		"# TYPE pg_sage_toil_minutes_saved gauge\n")
	b.WriteString(toil.String())
	b.WriteString("\n")
	b.WriteString("# HELP pg_sage_incidents_avoided_total " +
		"Incidents avoided by kind\n" +
		"# TYPE pg_sage_incidents_avoided_total counter\n")
	b.WriteString(incidents.String())
	b.WriteString("\n")
	up := 1
	if errToil != nil || errIncidents != nil {
		up = 0
		logWarn("metrics", "value metrics query failed: toil=%v incidents=%v",
			errToil, errIncidents)
	}
	b.WriteString("# HELP pg_sage_value_metrics_up " +
		"1 if the value metric queries succeeded on this scrape\n" +
		"# TYPE pg_sage_value_metrics_up gauge\n")
	fmt.Fprintf(b, "pg_sage_value_metrics_up %d\n\n", up)
}

func writeToilSeries(b *strings.Builder, ctx context.Context) error {
	rows, err := pool.Query(ctx, `SELECT COALESCE(d.name, ''),
		al.action_type, sum(al.toil_minutes_saved)::float8
		FROM sage.action_log al
		LEFT JOIN sage.databases d ON d.id = al.database_id
		WHERE al.outcome = 'success' AND al.toil_minutes_saved IS NOT NULL
		GROUP BY 1, 2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var database, feature string
		var minutes float64
		if err := rows.Scan(&database, &feature, &minutes); err != nil {
			return err
		}
		fmt.Fprintf(b, "pg_sage_toil_minutes_saved"+
			"{database=%q,feature=%q} %g\n", database, feature, minutes)
	}
	return rows.Err()
}

func writeIncidentSeries(b *strings.Builder, ctx context.Context) error {
	rows, err := pool.Query(ctx, `SELECT kind, count(*)::bigint
		FROM sage.incident_avoided GROUP BY kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return err
		}
		fmt.Fprintf(b, "pg_sage_incidents_avoided_total"+
			"{kind=%q} %d\n", kind, count)
	}
	return rows.Err()
}
