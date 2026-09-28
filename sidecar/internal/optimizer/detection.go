package optimizer

import (
	"regexp"
	"sort"
	"strings"
)

// IncludeCandidate identifies a query that would benefit from INCLUDE columns.
type IncludeCandidate struct {
	QueryID     int64
	HeapFetches int64
	SuggestCols []string // columns to INCLUDE (populated by LLM)
}

// PartialCandidate identifies a table where partial indexes may help.
type PartialCandidate struct {
	Column      string
	FilterValue string
	QueryPct    float64 // % of queries using this filter
	Selectivity float64 // estimated % of rows matching
}

// JoinPair groups queries by the tables they join.
type JoinPair struct {
	Left      string // schema.table
	Right     string // schema.table
	Condition string // join condition
	Queries   []QueryInfo
}

var (
	whereFilterRe = regexp.MustCompile(
		`(?i)(\w+)\s*=\s*(?:'([^']*)'|(\$\d+))`,
	)
	explicitJoinRe = regexp.MustCompile(
		`(?i)JOIN\s+([\w.]+)\s+(?:\w+\s+)?ON\s+(.+?)(?:\s+(?:JOIN|WHERE|GROUP|ORDER|LIMIT|$))`,
	)
	implicitJoinRe = regexp.MustCompile(
		`(?i)FROM\s+([\w.]+)(?:\s+\w+)?\s*,\s*([\w.]+)`,
	)
	implicitCondRe = regexp.MustCompile(
		`(?i)WHERE\s+.*?([\w.]+\s*=\s*[\w.]+)`,
	)
	groupByRe = regexp.MustCompile(`(?i)\bGROUP\s+BY\b`)
)

// DetectIncludeCandidates finds queries where Index Scan has high heap
// fetches, suggesting INCLUDE columns would help.
func DetectIncludeCandidates(
	plans []PlanSummary,
	threshold int64,
) []IncludeCandidate {
	var candidates []IncludeCandidate
	for _, p := range plans {
		if !strings.Contains(p.ScanType, "Index Scan") {
			continue
		}
		if p.HeapFetches <= threshold {
			continue
		}
		candidates = append(candidates, IncludeCandidate{
			QueryID:     p.QueryID,
			HeapFetches: p.HeapFetches,
		})
	}
	return candidates
}

// DetectPartialCandidates analyzes queries to find columns where >80%
// of queries use the same constant filter AND the filter has <20%
// selectivity per ColStat data.
func DetectPartialCandidates(
	queries []QueryInfo,
	colStats []ColStat,
) []PartialCandidate {
	if len(queries) == 0 {
		return nil
	}
	freqs := countFilterFrequencies(queries)
	threshold := 0.80
	total := float64(len(queries))

	var candidates []PartialCandidate
	for key, count := range freqs {
		pct := float64(count) / total
		if pct < threshold {
			continue
		}
		col, val := splitFilterKey(key)
		sel := lookupSelectivity(colStats, col)
		if sel >= 0.20 {
			continue
		}
		candidates = append(candidates, PartialCandidate{
			Column:      col,
			FilterValue: val,
			QueryPct:    pct,
			Selectivity: sel,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].QueryPct > candidates[j].QueryPct
	})
	return candidates
}

// extractWhereFilters extracts column=value pairs from a WHERE clause
// using simple regex matching.
func extractWhereFilters(query string) map[string]string {
	results := make(map[string]string)
	matches := whereFilterRe.FindAllStringSubmatch(query, -1)
	for _, m := range matches {
		col := strings.ToLower(m[1])
		val := m[2]
		if val == "" {
			val = m[3] // parameterized placeholder
		}
		results[col] = val
	}
	return results
}

// countFilterFrequencies counts how many queries use each (col, value)
// pair. Returns a map of "col\x00value" → count.
func countFilterFrequencies(queries []QueryInfo) map[string]int {
	freqs := make(map[string]int)
	for _, q := range queries {
		filters := extractWhereFilters(q.Text)
		for col, val := range filters {
			key := col + "\x00" + val
			freqs[key]++
		}
	}
	return freqs
}

// splitFilterKey splits a "col\x00value" key into column and value.
func splitFilterKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	if len(parts) != 2 {
		return key, ""
	}
	return parts[0], parts[1]
}

// lookupSelectivity returns the most common value frequency for a
// column from ColStat data. Returns 1.0 if not found.
func lookupSelectivity(colStats []ColStat, column string) float64 {
	for _, cs := range colStats {
		if strings.EqualFold(cs.Column, column) {
			if len(cs.MostCommonFreqs) > 0 {
				return cs.MostCommonFreqs[0]
			}
			return 1.0
		}
	}
	return 1.0
}

// DetectJoinPairs extracts join patterns from queries and groups them
// by table pair for cross-table optimization.
func DetectJoinPairs(queries []QueryInfo) []JoinPair {
	pairMap := make(map[string]*JoinPair)
	for _, q := range queries {
		extractExplicitJoins(q, pairMap)
		extractImplicitJoins(q, pairMap)
	}
	pairs := make([]JoinPair, 0, len(pairMap))
	for _, jp := range pairMap {
		pairs = append(pairs, *jp)
	}
	sort.Slice(pairs, func(i, j int) bool {
		return len(pairs[i].Queries) > len(pairs[j].Queries)
	})
	return pairs
}

// extractExplicitJoins finds JOIN ... ON patterns in query text.
func extractExplicitJoins(q QueryInfo, pairs map[string]*JoinPair) {
	fromRe := regexp.MustCompile(`(?i)FROM\s+([\w.]+)`)
	fromMatch := fromRe.FindStringSubmatch(q.Text)
	if fromMatch == nil {
		return
	}
	leftTable := strings.ToLower(fromMatch[1])
	matches := explicitJoinRe.FindAllStringSubmatch(q.Text, -1)
	for _, m := range matches {
		rightTable := strings.ToLower(m[1])
		condition := strings.TrimSpace(m[2])
		addJoinPair(pairs, leftTable, rightTable, condition, q)
	}
}

// extractImplicitJoins finds FROM a, b WHERE a.x = b.y patterns.
func extractImplicitJoins(q QueryInfo, pairs map[string]*JoinPair) {
	fromMatch := implicitJoinRe.FindStringSubmatch(q.Text)
	if fromMatch == nil {
		return
	}
	left := strings.ToLower(fromMatch[1])
	right := strings.ToLower(fromMatch[2])
	condition := ""
	condMatch := implicitCondRe.FindStringSubmatch(q.Text)
	if condMatch != nil {
		condition = strings.TrimSpace(condMatch[1])
	}
	addJoinPair(pairs, left, right, condition, q)
}

// addJoinPair inserts or updates a join pair entry in the map.
func addJoinPair(
	pairs map[string]*JoinPair,
	left, right, condition string,
	q QueryInfo,
) {
	if left > right {
		left, right = right, left
	}
	key := left + "|" + right
	jp, ok := pairs[key]
	if !ok {
		jp = &JoinPair{
			Left:      left,
			Right:     right,
			Condition: condition,
		}
		pairs[key] = jp
	}
	jp.Queries = append(jp.Queries, q)
}

// DetectMatViewCandidates finds queries that aggregate entire tables
// (GROUP BY on full scan) which cannot be fixed with indexes.
// Returns query IDs of candidates.
func DetectMatViewCandidates(
	queries []QueryInfo,
	plans []PlanSummary,
) []int64 {
	planByQID := make(map[int64]PlanSummary, len(plans))
	for _, p := range plans {
		planByQID[p.QueryID] = p
	}
	var ids []int64
	for _, q := range queries {
		p, ok := planByQID[q.QueryID]
		if !ok {
			continue
		}
		if !strings.Contains(p.ScanType, "Seq Scan") {
			continue
		}
		if !groupByRe.MatchString(q.Text) {
			continue
		}
		ids = append(ids, q.QueryID)
	}
	return ids
}

// DetectParamTuningNeeds analyzes plans for signals that indicate
// parameter tuning rather than index creation.
// Returns a map of signal name to recommendation string.
func DetectParamTuningNeeds(plans []PlanSummary) map[string]string {
	results := make(map[string]string)
	for _, p := range plans {
		if p.SortDisk > 0 {
			results["work_mem_sort"] =
				"work_mem: Sort spilling to disk"
		}
		if strings.Contains(p.Summary, "Hash Batches") {
			results["work_mem_hash"] =
				"work_mem: Hash join spilling"
		}
	}
	return results
}

// IsBRINCandidate returns true if a column has high enough physical
// correlation (>0.8) to benefit from a BRIN index.
func IsBRINCandidate(colStats []ColStat, column string) bool {
	for _, cs := range colStats {
		if strings.EqualFold(cs.Column, column) {
			return cs.Correlation > 0.8 || cs.Correlation < -0.8
		}
	}
	return false
}
