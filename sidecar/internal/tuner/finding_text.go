package tuner

import (
	"fmt"
	"strings"
)

// hasSpillSymptom returns true if any symptom indicates a
// disk sort or hash spill (plan-level detection).
func hasSpillSymptom(symptoms []PlanSymptom) bool {
	for _, s := range symptoms {
		switch s.Kind {
		case SymptomDiskSort, SymptomHashSpill:
			return true
		}
	}
	return false
}

func symptomNames(symptoms []PlanSymptom) []string {
	out := make([]string, len(symptoms))
	for i, s := range symptoms {
		out[i] = string(s.Kind)
	}
	return out
}

func buildTitle(symptoms []PlanSymptom) string {
	if len(symptoms) == 1 {
		return singleSymptomTitle(symptoms[0].Kind)
	}
	return multiSymptomTitle(symptoms)
}

func singleSymptomTitle(kind SymptomKind) string {
	switch kind {
	case SymptomDiskSort:
		return "Per-query tuning: increase work_mem " +
			"for disk-sorting query"
	case SymptomHashSpill:
		return "Per-query tuning: increase work_mem " +
			"for hash-spilling query"
	case SymptomHighPlanTime:
		return "Per-query tuning: force generic plan " +
			"for high-planning-time query"
	case SymptomBadNestedLoop:
		return "Per-query tuning: fix nested loop " +
			"join strategy"
	case SymptomSeqScanWithIndex:
		return "Per-query tuning: force index scan " +
			"for seq-scanning query"
	case SymptomParallelDisabled:
		return "Per-query tuning: enable parallel " +
			"workers for scan"
	case SymptomStatTempSpill:
		return "Per-query tuning: increase work_mem " +
			"for temp-spilling query"
	case SymptomIndexFilterInefficient:
		return "Per-query tuning: index filter discards most rows read"
	case SymptomORJoinFilter:
		return "Per-query tuning: OR join filter could be a UNION"
	default:
		return "Per-query tuning recommendation"
	}
}

func multiSymptomTitle(symptoms []PlanSymptom) string {
	kinds := make(map[SymptomKind]bool)
	for _, s := range symptoms {
		kinds[s.Kind] = true
	}
	var parts []string
	if kinds[SymptomDiskSort] || kinds[SymptomHashSpill] {
		parts = append(parts, "work_mem")
	}
	if kinds[SymptomHighPlanTime] {
		parts = append(parts, "generic plan")
	}
	if kinds[SymptomBadNestedLoop] {
		parts = append(parts, "join strategy")
	}
	if kinds[SymptomSeqScanWithIndex] {
		parts = append(parts, "index scan")
	}
	if kinds[SymptomParallelDisabled] {
		parts = append(parts, "parallel workers")
	}
	if kinds[SymptomStatTempSpill] {
		parts = append(parts, "work_mem")
	}
	if kinds[SymptomIndexFilterInefficient] {
		parts = append(parts, "index filter")
	}
	if kinds[SymptomORJoinFilter] {
		parts = append(parts, "OR join rewrite")
	}
	if len(parts) == 0 {
		return "Per-query tuning recommendation"
	}
	return fmt.Sprintf(
		"Per-query tuning: %s for query",
		strings.Join(parts, " + "),
	)
}

func buildRationale(prescriptions []Prescription) string {
	parts := make([]string, len(prescriptions))
	for i, p := range prescriptions {
		parts[i] = p.Rationale
	}
	return strings.Join(parts, "; ")
}
