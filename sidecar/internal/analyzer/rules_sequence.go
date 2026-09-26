package analyzer

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleSequenceExhaustion flags sequences approaching their max value.
// Handles both ascending and descending sequences.
// Warning at 75%, critical at 90%.
// INTEGER sequences (max ~2.1B) get extra emphasis.
func ruleSequenceExhaustion(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	var findings []Finding

	for _, seq := range current.Sequences {
		pct := sequenceUsedPct(seq)
		if pct < 75.0 {
			continue
		}

		severity := "warning"
		if pct >= 90.0 {
			severity = "critical"
		}

		ident := seq.SchemaName + "." + seq.SequenceName

		detail := map[string]any{
			"current_value": seq.LastValue,
			"max_value":     seq.MaxValue,
			"increment":     seq.IncrementBy,
			"usage_pct":     pct,
			"data_type":     seq.DataType,
		}

		recommendation := fmt.Sprintf(
			"Sequence %s is %.1f%% consumed.",
			ident, pct,
		)
		if seq.DataType == "integer" {
			recommendation += " Consider migrating to bigint."
		} else {
			recommendation += " Plan for sequence reset or range expansion."
		}

		findings = append(findings, Finding{
			Category:         "sequence_exhaustion",
			Severity:         severity,
			ObjectType:       "sequence",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Sequence %s is %.1f%% consumed (%s)",
				ident, pct, seq.DataType,
			),
			Detail:         detail,
			Recommendation: recommendation,
			ActionRisk:     "safe",
		})
	}
	return findings
}

// sequenceUsedPct returns the consumed share of a sequence's range in the
// direction it moves (C18/G1-B35). The collector's pct_used is
// last_value/max_value, which is 0 for descending sequences (negative
// max_value). For those the range is [type minimum, max_value]: the
// collector does not report min_value yet, so the data type's minimum is
// used, which under-reports a custom MINVALUE closer to zero. Collector
// follow-up: emit min_value, start_value and cycle so custom ranges and
// cycling sequences can be evaluated exactly.
func sequenceUsedPct(seq collector.SequenceStats) float64 {
	if seq.IncrementBy >= 0 {
		return seq.PctUsed
	}
	minValue, ok := sequenceTypeMin(seq.DataType)
	if !ok || float64(seq.MaxValue) <= minValue {
		return seq.PctUsed
	}
	span := float64(seq.MaxValue) - minValue
	used := float64(seq.MaxValue) - float64(seq.LastValue)
	if used <= 0 {
		return 0
	}
	return used / span * 100
}

func sequenceTypeMin(dataType string) (float64, bool) {
	switch dataType {
	case "smallint":
		return math.MinInt16, true
	case "integer":
		return math.MinInt32, true
	case "bigint":
		return math.MinInt64, true
	default:
		return 0, false
	}
}
