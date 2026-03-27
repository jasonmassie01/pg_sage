package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleTotalTimeHeavy flags queries consuming a large fraction of
// total database time, even if individual execution is fast.
// Example: 500ms mean * 1000 calls/sec = 500s/sec of CPU time.
func ruleTotalTimeHeavy(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if previous == nil {
		return nil
	}
	interval := current.CollectedAt.Sub(
		previous.CollectedAt,
	).Seconds()
	if interval <= 0 {
		return nil
	}

	// Threshold: query using >10% of wall clock.
	threshold := interval * 0.10 * 1000 // ms

	prevTotal := make(map[int64]float64)
	for _, q := range previous.Queries {
		prevTotal[q.QueryID] = q.TotalExecTime
	}

	var findings []Finding
	for _, q := range current.Queries {
		prev, ok := prevTotal[q.QueryID]
		if !ok {
			continue
		}
		delta := q.TotalExecTime - prev
		if delta <= 0 || delta <= threshold {
			continue
		}
		f := buildTotalTimeFinding(
			q, delta, interval,
		)
		findings = append(findings, f)
	}
	return findings
}

func buildTotalTimeFinding(
	q collector.QueryStats,
	delta, interval float64,
) Finding {
	pctOfWall := (delta / (interval * 1000)) * 100
	severity := "warning"
	if pctOfWall > 50 {
		severity = "critical"
	}
	ident := fmt.Sprintf("queryid:%d", q.QueryID)
	return Finding{
		Category:         "high_total_time",
		Severity:         severity,
		ObjectType:       "query",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Query consuming %.1f%% of wall clock "+
				"(%.0fms mean, %d calls)",
			pctOfWall, q.MeanExecTime, q.Calls,
		),
		Detail: map[string]any{
			"queryid":        q.QueryID,
			"query":          q.Query,
			"mean_exec_ms":   q.MeanExecTime,
			"calls":          q.Calls,
			"delta_total_ms": delta,
			"pct_wall_clock": pctOfWall,
		},
		Recommendation: "Optimize query or add supporting " +
			"index to reduce per-execution time.",
		ActionRisk: "safe",
	}
}
