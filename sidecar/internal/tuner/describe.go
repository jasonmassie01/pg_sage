package tuner

import (
	"fmt"
	"math"
	"strings"
)

// Describe turns a plan symptom into the two sentences the natural-language
// EXPLAIN shows as its deterministic floor (tuning-10): why the plan is
// slow, and what to do about it. It reads only the symptom, never the
// database, and names pg_hint_plan directives as options, never as actions
// pg_sage takes. An unknown kind is named with no recommendation.
func Describe(s PlanSymptom) (because, recommend string) {
	switch s.Kind {
	case SymptomDiskSort:
		return describeDiskSort(s)
	case SymptomHashSpill:
		return describeHashSpill(s)
	case SymptomHighPlanTime:
		return "planning takes a large share of the statement's time",
			"use prepared statements or a single array parameter (= ANY($1)), " +
				"so the plan is reused"
	case SymptomBadNestedLoop:
		return describeNestedLoop(s)
	case SymptomSeqScanWithIndex:
		return describeSeqScanWithIndex(s)
	case SymptomParallelDisabled:
		return describeSerialScan(s)
	case SymptomSortLimit:
		return describeSortLimit(s)
	case SymptomStatTempSpill:
		return "the statement writes temporary files on most calls",
			"raise work_mem for this statement until its temporary files stop"
	case SymptomStaleStats:
		return "statistics on " + relationOf(s) + " look stale, so row estimates are off",
			"ANALYZE " + relationOf(s)
	case SymptomIndexFilterInefficient:
		return describeIndexFilter(s)
	case SymptomORJoinFilter:
		return describeORJoin(s)
	}
	return fmt.Sprintf("%s on %s", s.Kind, nodeOf(s)), ""
}

func describeDiskSort(s PlanSymptom) (string, string) {
	kb, _ := s.Detail["sort_space_kb"].(int64)
	because := "sort spilled to disk"
	if kb > 0 {
		because = "sort spilled " + formatKB(kb) + " to disk"
	}
	return because, "raise work_mem for this statement above the spilled size " +
		"(SET LOCAL work_mem, or a Set(work_mem) hint), or add an index that returns " +
		"the rows already in order"
}

func describeHashSpill(s PlanSymptom) (string, string) {
	batches, _ := s.Detail["hash_batches"].(int64)
	peak, _ := s.Detail["peak_memory_kb"].(int64)
	because := "the hash table did not fit in work_mem"
	if batches > 1 {
		because = fmt.Sprintf("hash split into %d batches", batches)
		if peak > 0 {
			because += " at " + formatKB(peak) + " peak memory"
		}
	}
	return because, "raise work_mem for this statement so the hash fits in one batch"
}

func describeNestedLoop(s PlanSymptom) (string, string) {
	rows, _ := s.Detail["inner_rows_total"].(int64)
	loops, _ := s.Detail["loops"].(int64)
	because := "nested loop row estimate is off by 10x or more"
	if rows > 0 && loops > 0 {
		because += fmt.Sprintf("; its inner side read %d rows over %d loops", rows, loops)
	}
	recommend := "ANALYZE the joined tables so the estimate improves"
	if !joinMayHash(s) {
		recommend += "; the join has no equality between its inputs, " +
			"so a hash join cannot replace the nested loop"
	} else if len(s.JoinAliases) >= 2 {
		recommend += "; if it stays wrong, a hash join (HashJoin(" +
			strings.Join(s.JoinAliases, " ") + ")) reads the inner side once"
	}
	return because, recommend
}

func describeSeqScanWithIndex(s PlanSymptom) (string, string) {
	tableRows, _ := s.Detail["table_rows"].(int64)
	because := "seq scan of " + relationOf(s)
	if tableRows > 0 {
		because += fmt.Sprintf(" (about %d rows)", tableRows)
	}
	index := s.IndexName
	if index == "" {
		index = "an index"
	}
	because += " keeps few rows although " + index + " covers its filter"
	return because, "find out why the planner skips " + index + " (stale statistics, a " +
		"type or collation mismatch in the filter, or random_page_cost too high)"
}

func describeSerialScan(s PlanSymptom) (string, string) {
	tableRows, _ := s.Detail["table_rows"].(int64)
	because := "serial seq scan of " + relationOf(s)
	if tableRows > 0 {
		because += fmt.Sprintf(" (about %d rows) with no parallel workers", tableRows)
	}
	return because, "check max_parallel_workers_per_gather and the parallel cost " +
		"settings, or read fewer rows with an index"
}

func describeSortLimit(s PlanSymptom) (string, string) {
	sortRows, _ := s.Detail["sort_rows"].(int64)
	limitRows, _ := s.Detail["limit_rows"].(int64)
	because := "the whole input is sorted to return a few rows"
	if sortRows > 0 && limitRows > 0 {
		because = fmt.Sprintf("sort of %d rows feeds a LIMIT of %d", sortRows, limitRows)
	}
	return because, "add an index on the ORDER BY columns so the LIMIT reads only " +
		"the rows it returns"
}

func describeIndexFilter(s PlanSymptom) (string, string) {
	share, _ := s.Detail["removed_share"].(float64)
	because := "the index scan's filter discards most of the rows it reads"
	if share > 0 {
		subject := "the bitmap scan of " + relationOf(s)
		if s.IndexName != "" {
			subject = "index " + s.IndexName
		}
		because = fmt.Sprintf("%s reads rows its filter then discards (%.0f%%)", subject,
			math.Floor(share*100+1e-9))
	}
	if p := prescribeIndexFilter(s); p != nil && s.Detail != nil {
		return because, p.Rationale
	}
	return because, "an index that also covers the filter would read only the kept rows"
}

func describeORJoin(s PlanSymptom) (string, string) {
	removed, _ := s.Detail["rows_removed"].(int64)
	because := "an OR join filter is applied after the join"
	if removed > 0 {
		because = fmt.Sprintf("the nested loop's OR join filter discards %d rows after "+
			"the join", removed)
	}
	return because, "rewrite the OR as UNION (or UNION ALL with an exclusion predicate) " +
		"so each arm can use its own index; pg_sage does not rewrite statements"
}

// formatKB prints a size given in kB as KB, MB or GB with one decimal.
func formatKB(kb int64) string {
	switch {
	case kb >= 1024*1024:
		return fmt.Sprintf("%.1f GB", float64(kb)/(1024*1024))
	case kb >= 1024:
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	}
	return fmt.Sprintf("%d KB", kb)
}

func relationOf(s PlanSymptom) string {
	switch {
	case s.Schema != "" && s.RelationName != "":
		return s.Schema + "." + s.RelationName
	case s.RelationName != "":
		return s.RelationName
	}
	return "the table"
}

func nodeOf(s PlanSymptom) string {
	if s.NodeType == "" {
		return "the plan"
	}
	return s.NodeType
}
