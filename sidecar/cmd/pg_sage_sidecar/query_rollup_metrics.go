package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// writeQueryRollupMetrics emits the hourly per-query rollup's hours by
// result and its lag per database (evid-01a). A growing failed series or
// a lag above a few hours means the rollup is not keeping up.
func writeQueryRollupMetrics(b *strings.Builder, hours []querystore.RollupHourCount,
	lags []querystore.RollupLag) {
	metricsreg.Header(b, "pg_sage_query_rollup_hours_total")
	for _, h := range hours {
		fmt.Fprintf(b, "pg_sage_query_rollup_hours_total{database=%q,result=%q} %d\n",
			h.Database, h.Result, h.Count)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_query_rollup_lag_hours")
	for _, l := range lags {
		fmt.Fprintf(b, "pg_sage_query_rollup_lag_hours{database=%q} %g\n", l.Database,
			l.Hours)
	}
	b.WriteString("\n")
}
