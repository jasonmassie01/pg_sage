package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// planNode represents a node in EXPLAIN (FORMAT JSON) output.
type planNode struct {
	NodeType string     `json:"Node Type"`
	PlanRows int64      `json:"Plan Rows"`
	SortKey  []string   `json:"Sort Key"`
	Plans    []planNode `json:"Plans"`
}

// ExplainEntry holds a cached explain plan for analysis.
type ExplainEntry struct {
	QueryID   int64
	QueryText string
	PlanJSON  []byte
}

// ruleSortWithoutIndex analyzes plan JSON entries for Sort nodes
// feeding Limit nodes where the sort processes far more rows than
// the limit returns. This is a pure function for testability.
func ruleSortWithoutIndex(entries []ExplainEntry) []Finding {
	var findings []Finding
	for _, e := range entries {
		f := checkSortLimit(e)
		if f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

// checkSortLimit inspects a single plan for the Sort+Limit pattern.
func checkSortLimit(entry ExplainEntry) *Finding {
	root, ok := parsePlanRoot(entry.PlanJSON)
	if !ok {
		return nil
	}
	sortRows, limitRows, sortKey, found := findSortLimit(root)
	if !found {
		return nil
	}
	return buildSortFinding(entry, sortRows, limitRows, sortKey)
}

// parsePlanRoot extracts the root plan node from EXPLAIN JSON.
func parsePlanRoot(planJSON []byte) (planNode, bool) {
	var wrapper []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &wrapper); err != nil {
		return planNode{}, false
	}
	if len(wrapper) == 0 {
		return planNode{}, false
	}
	return wrapper[0].Plan, true
}

// findSortLimit walks the plan tree looking for a Limit node
// whose child is a Sort node with Plan Rows >> Limit's Plan Rows.
// Returns (sortRows, limitRows, sortKey, found).
func findSortLimit(
	node planNode,
) (int64, int64, []string, bool) {
	if node.NodeType == "Limit" {
		for _, child := range node.Plans {
			if child.NodeType == "Sort" {
				ratio := safeRatio(child.PlanRows, node.PlanRows)
				if ratio >= 10 {
					return child.PlanRows, node.PlanRows,
						child.SortKey, true
				}
			}
		}
	}
	for _, child := range node.Plans {
		if s, l, sk, ok := findSortLimit(child); ok {
			return s, l, sk, true
		}
	}
	return 0, 0, nil, false
}

// parseSortKey strips table qualifiers and sort directions from a
// single Sort Key entry, returning just the column name.
func parseSortKey(key string) string {
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		key = key[dot+1:]
	}
	suffixes := []string{
		" NULLS FIRST", " NULLS LAST", " DESC", " ASC",
	}
	for changed := true; changed; {
		changed = false
		for _, s := range suffixes {
			trimmed := strings.TrimSuffix(key, s)
			if trimmed != key {
				key = trimmed
				changed = true
			}
		}
	}
	return strings.TrimSpace(key)
}

func safeRatio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func buildSortFinding(
	entry ExplainEntry,
	sortRows, limitRows int64,
	sortKey []string,
) *Finding {
	ratio := safeRatio(sortRows, limitRows)
	ident := fmt.Sprintf("queryid:%d", entry.QueryID)
	detail := map[string]any{
		"queryid":    entry.QueryID,
		"query":      entry.QueryText,
		"sort_rows":  sortRows,
		"limit_rows": limitRows,
		"ratio":      ratio,
	}
	rec := "Add an index matching the ORDER BY " +
		"columns to avoid sorting."
	if len(sortKey) > 0 {
		cols := make([]string, len(sortKey))
		for i, k := range sortKey {
			cols[i] = parseSortKey(k)
		}
		detail["sort_columns"] = cols
		rec = fmt.Sprintf(
			"Add an index on (%s) to avoid sorting.",
			strings.Join(cols, ", "),
		)
	}
	return &Finding{
		Category:         "sort_without_index",
		Severity:         "warning",
		ObjectType:       "query",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Sort processes %d rows for LIMIT %d (%.0fx waste)",
			sortRows, limitRows, ratio,
		),
		Detail:         detail,
		Recommendation: rec,
		ActionRisk:     "safe",
	}
}

// checkSortWithoutIndex loads recent explain plans and checks for
// Sort+Limit patterns where the sort processes far more rows than
// the limit returns.
func (a *Analyzer) checkSortWithoutIndex(
	ctx context.Context,
) []Finding {
	rows, err := a.pool.Query(ctx, `
		SELECT DISTINCT ON (queryid)
			queryid, query_text, plan_json
		FROM sage.explain_cache
		WHERE captured_at > now() - interval '1 day'
		ORDER BY queryid, captured_at DESC`)
	if err != nil {
		a.logFn(
			"ERROR",
			"analyzer: sort_without_index query: %v", err,
		)
		return nil
	}
	defer rows.Close()

	var entries []ExplainEntry
	for rows.Next() {
		var e ExplainEntry
		if err := rows.Scan(
			&e.QueryID, &e.QueryText, &e.PlanJSON,
		); err != nil {
			a.logFn(
				"WARN",
				"analyzer: scan explain entry: %v", err,
			)
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		a.logFn(
			"ERROR",
			"analyzer: iterate explain entries: %v", err,
		)
	}
	return ruleSortWithoutIndex(entries)
}
