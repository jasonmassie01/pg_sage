package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleAutovacuumTuning recommends a per-table autovacuum_vacuum_scale_factor
// for large, write-heavy tables. PostgreSQL's global default scale factor
// (0.20) means a 10M-row table accumulates ~2M dead tuples before
// autovacuum runs — far too coarse. The fix is a durable per-table
// reloption and is fully reversible via ALTER TABLE ... RESET, so it is a
// SAFE-tier action (A1). It complements ruleTableBloat (a one-off VACUUM):
// this rule fixes the *cause* (mis-tuned autovacuum), not the symptom.
func ruleAutovacuumTuning(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if current == nil {
		return nil
	}
	minRows := int64(cfg.Analyzer.AutovacuumTuneMinRows)
	if minRows <= 0 {
		minRows = 1_000_000
	}

	var findings []Finding
	for _, t := range current.Tables {
		live := t.NLiveTup
		if live < minRows {
			continue // tuning small tables is pointless
		}
		// Write-heavy heuristic: lifetime updates+deletes exceed the
		// current live-row count, i.e. the table churns enough that the
		// default scale factor lets too many dead tuples build up.
		if t.NTupUpd+t.NTupDel < live {
			continue
		}

		target := targetScaleFactor(live)
		ident := t.SchemaName + "." + t.RelName
		q := sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)

		findings = append(findings, Finding{
			Category:         "autovacuum_tuning",
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Table %s should use autovacuum_vacuum_scale_factor=%.2f",
				ident, target),
			Detail: map[string]any{
				"n_live_tup":               live,
				"n_dead_tup":               t.NDeadTup,
				"lifetime_updates":         t.NTupUpd,
				"lifetime_deletes":         t.NTupDel,
				"recommended_scale_factor": target,
				"default_scale_factor":     0.20,
			},
			Recommendation: fmt.Sprintf(
				"Set autovacuum_vacuum_scale_factor=%.2f on %s so autovacuum "+
					"triggers before dead tuples accumulate; the default 0.20 "+
					"is too coarse for a table this large and write-heavy. "+
					"Reversible via ALTER TABLE ... RESET.",
				target, ident),
			RecommendedSQL: fmt.Sprintf(
				"ALTER TABLE %s SET (autovacuum_vacuum_scale_factor = %.2f);",
				q, target),
			RollbackSQL: fmt.Sprintf(
				"ALTER TABLE %s RESET (autovacuum_vacuum_scale_factor);", q),
			ActionRisk: "safe",
		})
	}
	return findings
}

// targetScaleFactor scales the recommended autovacuum_vacuum_scale_factor
// down as the table grows, keeping the absolute dead-tuple count that
// triggers autovacuum roughly bounded instead of growing with the table.
func targetScaleFactor(liveRows int64) float64 {
	switch {
	case liveRows >= 50_000_000:
		return 0.01
	case liveRows >= 10_000_000:
		return 0.02
	default:
		return 0.05
	}
}
