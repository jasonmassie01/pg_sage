package tuner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Prescribe returns a hint directive for a symptom, or nil if
// the symptom kind is unknown.
func Prescribe(
	symptom PlanSymptom, cfg TunerConfig,
) *Prescription {
	switch symptom.Kind {
	case SymptomDiskSort:
		return prescribeDiskSort(symptom, cfg)
	case SymptomHashSpill:
		return prescribeHashSpill(symptom, cfg)
	case SymptomHighPlanTime:
		return prescribeHighPlanTime()
	case SymptomBadNestedLoop:
		return prescribeBadNestedLoop(symptom)
	case SymptomSeqScanWithIndex:
		return prescribeIndexScan(symptom)
	case SymptomParallelDisabled:
		return prescribeParallel()
	case SymptomSortLimit:
		return prescribeSortLimit(symptom)
	case SymptomStatTempSpill:
		return prescribeStatTempSpill(symptom)
	case SymptomStaleStats:
		return prescribeStaleStats(symptom)
	case SymptomIndexFilterInefficient:
		return prescribeIndexFilter(symptom)
	case SymptomORJoinFilter:
		return prescribeORJoin(symptom)
	default:
		return nil
	}
}

// prescribeStaleStats builds a Prescription whose AnalyzeTarget
// identifies the table needing ANALYZE. HintDirective stays
// empty so CombineHints emits nothing for this symptom — the
// tuner handles stale-stats symptoms outside the hint flow by
// producing a dedicated table-level finding.
func prescribeStaleStats(s PlanSymptom) *Prescription {
	schema := s.Schema
	if schema == "" {
		schema = "public"
	}
	canonical := schema + "." + s.RelationName
	return &Prescription{
		Symptom:       SymptomStaleStats,
		AnalyzeTarget: canonical,
		Rationale: fmt.Sprintf(
			"statistics stale on %s — ANALYZE recommended",
			canonical,
		),
	}
}

func prescribeDiskSort(
	s PlanSymptom, cfg TunerConfig,
) *Prescription {
	kb, _ := s.Detail["sort_space_kb"].(int64)
	mem := CalcWorkMem(kb, cfg.WorkMemMaxMB)
	return &Prescription{
		Symptom:       SymptomDiskSort,
		HintDirective: fmtSetWorkMem(mem),
		Rationale: fmt.Sprintf(
			"sort spilled %d KB to disk", kb,
		),
	}
}

func prescribeHashSpill(
	s PlanSymptom, cfg TunerConfig,
) *Prescription {
	peak, _ := s.Detail["peak_memory_kb"].(int64)
	batches, _ := s.Detail["hash_batches"].(int64)
	mem := CalcWorkMemHash(peak, batches, cfg.WorkMemMaxMB)
	return &Prescription{
		Symptom:       SymptomHashSpill,
		HintDirective: fmtSetWorkMem(mem),
		Rationale: fmt.Sprintf(
			"hash used %d batches, peak %d KB",
			batches, peak,
		),
	}
}

// prescribeHighPlanTime prescribes no hint (tuning-11): a forced generic
// plan does nothing for simple-protocol statements and can be worse on
// skewed data, so the statement gets query_parameterization advice. A
// plan_cache_mode hint may return only when a hinted EXPLAIN shows it
// changes the plan.
func prescribeHighPlanTime() *Prescription {
	return &Prescription{
		Symptom:   SymptomHighPlanTime,
		Rationale: "planning time exceeds threshold",
	}
}

// hintIdent is an identifier pg_hint_plan accepts unquoted. Anything
// else (mixed case, spaces, quotes) is skipped rather than emitted raw.
var hintIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// prescribeBadNestedLoop forces a hash join of the relations the nested
// loop joins. pg_hint_plan join hints need two or more relation aliases,
// so with fewer (or with an alias it cannot take unquoted) no hint is
// prescribed; never HashJoin() or HashJoin(<one alias>). A join the
// scanner found no equality for (hashable_join false: a pure OR, no join
// condition) cannot be hash-joined, so no hint either (market-10b).
func prescribeBadNestedLoop(s PlanSymptom) *Prescription {
	if len(s.JoinAliases) < 2 || !joinMayHash(s) {
		return nil
	}
	for _, a := range s.JoinAliases {
		if !hintIdent.MatchString(a) {
			return nil
		}
	}
	innerRows, _ := s.Detail["inner_rows_total"].(int64)
	loops, _ := s.Detail["loops"].(int64)
	return &Prescription{
		Symptom:       SymptomBadNestedLoop,
		HintDirective: "HashJoin(" + strings.Join(s.JoinAliases, " ") + ")",
		Rationale: fmt.Sprintf(
			"nested loop row estimate off by >10x; inner side read %d rows over %d loops",
			innerRows, loops),
	}
}

// prescribeIndexScan names the usable index the symptom found; without
// one there is nothing to force.
func prescribeIndexScan(s PlanSymptom) *Prescription {
	alias := s.Alias
	if alias == "" {
		alias = s.RelationName
	}
	if !hintIdent.MatchString(alias) || !hintIdent.MatchString(s.IndexName) {
		return nil
	}
	return &Prescription{
		Symptom:       SymptomSeqScanWithIndex,
		HintDirective: fmt.Sprintf("IndexScan(%s %s)", alias, s.IndexName),
		Rationale:     "seq scan on a large table with a usable index on its filter",
	}
}

func prescribeParallel() *Prescription {
	return &Prescription{
		Symptom:       SymptomParallelDisabled,
		HintDirective: `Set(max_parallel_workers_per_gather "4")`,
		Rationale:     "parallel workers not planned for scan",
	}
}

func prescribeSortLimit(s PlanSymptom) *Prescription {
	sortRows, _ := s.Detail["sort_rows"].(int64)
	limitRows, _ := s.Detail["limit_rows"].(int64)
	return &Prescription{
		Symptom: SymptomSortLimit,
		Rationale: fmt.Sprintf(
			"Sort processes %d rows for LIMIT %d; "+
				"add index on sort columns to avoid "+
				"full table sort",
			sortRows, limitRows,
		),
	}
}

// prescribeStatTempSpill emits the work_mem the tuner sized for the
// symptom from the interval's per-call spill (tuning-03, see
// sizeStatSpillWorkMem); a symptom without a sized hint prescribes nothing.
func prescribeStatTempSpill(s PlanSymptom) *Prescription {
	mb, _ := s.Detail["hint_mb"].(int)
	if mb <= 0 {
		return nil
	}
	return &Prescription{
		Symptom:       SymptomStatTempSpill,
		HintDirective: fmtSetWorkMem(mb),
		Rationale:     statSpillRationale(s.Detail),
	}
}

func fmtSetWorkMem(mb int) string {
	return fmt.Sprintf(`Set(work_mem "%dMB")`, mb)
}

// CalcWorkMem computes work_mem in MB from sort space in KB.
// Result is at least 64 MB and at most maxMB.
func CalcWorkMem(sortSpaceKB int64, maxMB int) int {
	mb := int(sortSpaceKB * 2 / 1024)
	return clampWorkMem(mb, maxMB)
}

// CalcWorkMemHash computes work_mem from hash spill stats.
// Result is at least 64 MB and at most maxMB.
func CalcWorkMemHash(
	peakMemKB, batches int64, maxMB int,
) int {
	mb := int(peakMemKB * batches * 2 / 1024)
	return clampWorkMem(mb, maxMB)
}

func clampWorkMem(mb, maxMB int) int {
	if mb < 64 {
		mb = 64
	}
	if maxMB > 0 && mb > maxMB {
		mb = maxMB
	}
	return mb
}

// CombineHints joins prescriptions into a single hint string.
// When both disk_sort and hash_spill set work_mem, the larger
// value wins.
func CombineHints(prescriptions []Prescription) string {
	var workMemMB int
	var hasWorkMem bool
	var others []string

	for _, p := range prescriptions {
		// Split combined directives like
		// "Set(work_mem ...) IndexScan(...)" into parts.
		parts := splitHintDirectives(p.HintDirective)
		if len(parts) <= 1 {
			parts = []string{p.HintDirective}
		}
		for _, d := range parts {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if mb, ok := extractWorkMemMB(d); ok {
				hasWorkMem = true
				if mb > workMemMB {
					workMemMB = mb
				}
				continue
			}
			others = append(others, d)
		}
	}

	var parts []string
	if hasWorkMem {
		parts = append(parts, fmtSetWorkMem(workMemMB))
	}
	parts = append(parts, others...)
	return strings.Join(parts, " ")
}

var workMemRe = regexp.MustCompile(
	`Set\(work_mem "(\d+)MB"\)`,
)

func extractWorkMemMB(directive string) (int, bool) {
	m := workMemRe.FindStringSubmatch(directive)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return v, true
}
