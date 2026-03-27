package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ioWaitRatio computes the fraction of total query execution time spent
// on block I/O (reads + writes) across all tracked queries. Returns 0
// when there is no meaningful execution time. The blk_read_time and
// blk_write_time columns in pg_stat_database require track_io_timing=on;
// when disabled they stay at 0, so this naturally returns 0.
func ioWaitRatio(snap *collector.Snapshot) float64 {
	var totalExec, totalIO float64
	for _, q := range snap.Queries {
		totalExec += q.TotalExecTime
		totalIO += q.BlkReadTime + q.BlkWriteTime
	}
	if totalExec <= 0 {
		return 0
	}
	ratio := totalIO / totalExec
	if ratio > 1 {
		ratio = 1
	}
	return ratio
}

// ioSaturated returns true when the system is I/O-bound: combined
// block read + write time exceeds 50% of total query execution time.
func ioSaturated(snap *collector.Snapshot) bool {
	return ioWaitRatio(snap) > 0.50
}

// ruleTableBloat flags tables where the dead tuple ratio exceeds the
// configured threshold. When the system is I/O-saturated, findings
// are downgraded from "warning" to "info" to avoid recommending
// aggressive vacuum on an already I/O-bound system.
func ruleTableBloat(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	threshold := float64(cfg.Analyzer.TableBloatDeadTuplePct) / 100.0
	var findings []Finding

	minRows := int64(cfg.Analyzer.TableBloatMinRows)
	saturated := ioSaturated(current)

	for _, t := range current.Tables {
		total := t.NLiveTup + t.NDeadTup
		if total < minRows {
			continue
		}
		if total == 0 {
			total = 1
		}
		deadRatio := float64(t.NDeadTup) / float64(total)
		if deadRatio <= threshold {
			continue
		}

		severity := "warning"
		recommendation := "Run VACUUM to reclaim dead tuple space."
		if saturated {
			severity = "info"
			recommendation = "Table needs vacuum but system is " +
				"I/O-saturated; deferring until I/O pressure drops."
		}

		ident := t.SchemaName + "." + t.RelName
		findings = append(findings, Finding{
			Category:         "table_bloat",
			Severity:         severity,
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Table %s has %.1f%% dead tuples",
				ident, deadRatio*100,
			),
			Detail: map[string]any{
				"n_live_tup":    t.NLiveTup,
				"n_dead_tup":    t.NDeadTup,
				"dead_ratio":    deadRatio,
				"last_vacuum":   t.LastVacuum,
				"io_saturated":  saturated,
				"io_wait_ratio": ioWaitRatio(current),
			},
			Recommendation: recommendation,
			RecommendedSQL: fmt.Sprintf("VACUUM %s;", ident),
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleXIDWraparound flags databases approaching XID wraparound.
// xidAge is obtained from: SELECT age(datfrozenxid) FROM pg_database
// WHERE datname = current_database().
func ruleXIDWraparound(xidAge int64, cfg *config.Config) []Finding {
	var findings []Finding

	if xidAge < cfg.Analyzer.XIDWraparoundWarning {
		return findings
	}

	severity := "warning"
	if xidAge >= cfg.Analyzer.XIDWraparoundCritical {
		severity = "critical"
	}

	findings = append(findings, Finding{
		Category:         "xid_wraparound",
		Severity:         severity,
		ObjectType:       "database",
		ObjectIdentifier: "current_database",
		Title: fmt.Sprintf(
			"XID age is %d (wraparound risk)",
			xidAge,
		),
		Detail: map[string]any{
			"xid_age":            xidAge,
			"warning_threshold":  cfg.Analyzer.XIDWraparoundWarning,
			"critical_threshold": cfg.Analyzer.XIDWraparoundCritical,
		},
		Recommendation: "Run VACUUM FREEZE on high-age tables.",
		RecommendedSQL: "VACUUM FREEZE;",
		ActionRisk:     "safe",
	})
	return findings
}
