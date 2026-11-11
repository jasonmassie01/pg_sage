package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// SequenceRunwayCategory is the runway finding category that holds a
// sequence's time-to-limit and its owning column's type limit
// (runway.CategorySequence; runway imports this package, so the value is
// pinned by a runway test rather than imported).
const SequenceRunwayCategory = "forecast_sequence_runway"

// ruleSequenceExhaustion flags sequences approaching the end of their
// range: warning at 75%, critical at 90%. Usage is the collector's
// pct_used, the consumed share of [min_value, max_value] in the direction
// of travel, so ascending and descending sequences with custom ranges are
// judged alike. CYCLE sequences wrap by design and are skipped, as the
// runway skips them. The rate and the owning column's limit belong to the
// runway finding for the same sequence, which the finding points to.
func ruleSequenceExhaustion(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	var findings []Finding
	for _, seq := range current.Sequences {
		if seq.Cycle || seq.PctUsed < 75.0 {
			continue
		}
		findings = append(findings, sequenceExhaustionFinding(seq))
	}
	return findings
}

func sequenceExhaustionFinding(seq collector.SequenceStats) Finding {
	pct := seq.PctUsed
	severity := "warning"
	if pct >= 90.0 {
		severity = "critical"
	}
	ident := seq.SchemaName + "." + seq.SequenceName
	recommendation := fmt.Sprintf("Sequence %s is %.1f%% consumed.", ident, pct)
	if seq.DataType == "integer" {
		recommendation += " Consider migrating to bigint."
	} else {
		recommendation += " Plan for sequence reset or range expansion."
	}
	recommendation += fmt.Sprintf(" The time to the limit and the owning column's "+
		"type limit are in the %s finding for this sequence.", SequenceRunwayCategory)
	return Finding{
		Category:         "sequence_exhaustion",
		Severity:         severity,
		ObjectType:       "sequence",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Sequence %s is %.1f%% consumed (%s)", ident, pct, seq.DataType,
		),
		Detail: map[string]any{
			"current_value":   seq.LastValue,
			"min_value":       seq.MinValue,
			"max_value":       seq.MaxValue,
			"increment":       seq.IncrementBy,
			"cycle":           seq.Cycle,
			"usage_pct":       pct,
			"data_type":       seq.DataType,
			"runway_category": SequenceRunwayCategory,
		},
		Recommendation: recommendation,
		ActionRisk:     "safe",
	}
}
