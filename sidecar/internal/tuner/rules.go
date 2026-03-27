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
	default:
		return nil
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

func prescribeHighPlanTime() *Prescription {
	return &Prescription{
		Symptom: SymptomHighPlanTime,
		HintDirective: "Set(plan_cache_mode " +
			"'force_generic_plan')",
		Rationale: "planning time exceeds threshold",
	}
}

func prescribeBadNestedLoop(s PlanSymptom) *Prescription {
	alias := s.Alias
	if alias == "" {
		alias = s.RelationName
	}
	return &Prescription{
		Symptom:       SymptomBadNestedLoop,
		HintDirective: fmt.Sprintf("HashJoin(%s)", alias),
		Rationale:     "nested loop row estimate off by >10x",
	}
}

func prescribeIndexScan(s PlanSymptom) *Prescription {
	alias := s.Alias
	if alias == "" {
		alias = s.RelationName
	}
	idx := s.IndexName
	if idx == "" {
		return &Prescription{
			Symptom: SymptomSeqScanWithIndex,
			HintDirective: fmt.Sprintf(
				"IndexScan(%s)", alias,
			),
			Rationale: "seq scan on indexed relation",
		}
	}
	return &Prescription{
		Symptom: SymptomSeqScanWithIndex,
		HintDirective: fmt.Sprintf(
			"IndexScan(%s %s)", alias, idx,
		),
		Rationale: "seq scan on indexed relation",
	}
}

func prescribeParallel() *Prescription {
	return &Prescription{
		Symptom: SymptomParallelDisabled,
		HintDirective: "Set(max_parallel_workers_per_gather" +
			" '4')",
		Rationale: "parallel workers not planned for scan",
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

func fmtSetWorkMem(mb int) string {
	return fmt.Sprintf("Set(work_mem '%dMB')", mb)
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
		if mb, ok := extractWorkMemMB(p.HintDirective); ok {
			hasWorkMem = true
			if mb > workMemMB {
				workMemMB = mb
			}
			continue
		}
		others = append(others, p.HintDirective)
	}

	var parts []string
	if hasWorkMem {
		parts = append(parts, fmtSetWorkMem(workMemMB))
	}
	parts = append(parts, others...)
	return strings.Join(parts, " ")
}

var workMemRe = regexp.MustCompile(
	`Set\(work_mem '(\d+)MB'\)`,
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
