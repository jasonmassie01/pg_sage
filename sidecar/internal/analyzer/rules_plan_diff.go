package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// planPair holds current and previous EXPLAIN plans for comparison.
type planPair struct {
	QueryID      int64
	QueryText    string
	CurrentPlan  []byte
	CurrentCost  float64
	CurrentTime  float64
	PreviousPlan []byte
	PreviousCost float64
	PreviousTime float64
}

// planDiffResult captures the delta between two plans.
type planDiffResult struct {
	CostRatio     float64
	NodeChanges   []string
	NewDiskSpills bool
}

// nodeEntry is a flattened plan tree node with depth.
type nodeEntry struct {
	Depth    int
	NodeType string
}

// rulePlanRegression iterates plan pairs and emits findings for
// regressions detected by diffPlans.
func rulePlanRegression(pairs []planPair) []Finding {
	var findings []Finding
	for _, p := range pairs {
		result := diffPlans(
			p.CurrentPlan, p.PreviousPlan,
			p.CurrentCost, p.PreviousCost,
		)
		if result == nil {
			continue
		}
		f := buildPlanRegressionFinding(p, result)
		if f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

// buildPlanRegressionFinding constructs a Finding from a detected
// plan regression.
func buildPlanRegressionFinding(
	p planPair, r *planDiffResult,
) *Finding {
	sev := severityFromCostRatio(r.CostRatio)
	if sev == "info" && (len(r.NodeChanges) > 0 || r.NewDiskSpills) {
		sev = "warning"
	}
	if sev == "info" {
		return nil
	}
	return &Finding{
		Category:         "plan_regression",
		Severity:         sev,
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", p.QueryID),
		Title: fmt.Sprintf(
			"Plan regression: %.1fx cost increase for queryid %d",
			r.CostRatio, p.QueryID,
		),
		Detail: map[string]any{
			"queryid":          p.QueryID,
			"query":            p.QueryText,
			"cost_ratio":       r.CostRatio,
			"previous_cost":    p.PreviousCost,
			"current_cost":     p.CurrentCost,
			"node_changes":     r.NodeChanges,
			"new_disk_spills":  r.NewDiskSpills,
			"previous_summary": buildPlanSummary(p.PreviousPlan),
			"current_summary":  buildPlanSummary(p.CurrentPlan),
		},
		Recommendation: fmt.Sprintf(
			"Query %d plan cost increased %.1fx. %s",
			p.QueryID, r.CostRatio,
			"Investigate parameter changes or stale statistics.",
		),
	}
}

// diffPlans compares two EXPLAIN plans and returns a result when a
// regression is detected. Returns nil for trivial or improved plans.
func diffPlans(
	current, previous []byte,
	currentCost, previousCost float64,
) *planDiffResult {
	if currentCost < 1.0 && previousCost < 1.0 {
		return nil
	}
	if previousCost <= 0 {
		return nil
	}
	ratio := currentCost / previousCost
	if ratio < 1.0 {
		return nil
	}
	curNodes := collectNodeTypes(current)
	prevNodes := collectNodeTypes(previous)
	if curNodes == nil && prevNodes == nil {
		return nil
	}
	changes := detectNodeChanges(curNodes, prevNodes)
	newSpills := hasDiskSpill(current) && !hasDiskSpill(previous)

	if ratio >= 2.0 {
		return &planDiffResult{ratio, changes, newSpills}
	}
	if ratio >= 1.5 && (len(changes) > 0 || newSpills) {
		return &planDiffResult{ratio, changes, newSpills}
	}
	return nil
}

// collectNodeTypes walks the plan JSON tree and returns a flat list
// of (depth, nodeType) entries in pre-order.
func collectNodeTypes(planJSON []byte) []nodeEntry {
	root, ok := parsePlanRoot(planJSON)
	if !ok {
		return nil
	}
	var entries []nodeEntry
	walkNodes(root, 0, &entries)
	return entries
}

func walkNodes(node planNode, depth int, out *[]nodeEntry) {
	*out = append(*out, nodeEntry{Depth: depth, NodeType: node.NodeType})
	for _, child := range node.Plans {
		walkNodes(child, depth+1, out)
	}
}

// detectNodeChanges compares current and previous node lists and
// returns human-readable descriptions of structural downgrades.
func detectNodeChanges(
	current, previous []nodeEntry,
) []string {
	prevSet := make(map[string]int)
	for _, e := range previous {
		prevSet[e.NodeType]++
	}
	curSet := make(map[string]int)
	for _, e := range current {
		curSet[e.NodeType]++
	}
	var changes []string
	type downgrade struct{ worse, better string }
	downgrades := []downgrade{
		{"Seq Scan", "Index Scan"},
		{"Seq Scan", "Index Only Scan"},
		{"Hash Join", "Nested Loop"},
		{"Sort", "Index Scan"},
		{"Bitmap Heap Scan", "Index Scan"},
	}
	for _, d := range downgrades {
		if curSet[d.worse] > prevSet[d.worse] &&
			curSet[d.better] < prevSet[d.better] {
			changes = append(changes, d.better+" \u2192 "+d.worse)
		}
	}
	if curSet["Seq Scan"] > prevSet["Seq Scan"] {
		for _, prev := range previous {
			if strings.Contains(prev.NodeType, "Index") {
				found := false
				for _, cur := range current {
					if cur.NodeType == prev.NodeType {
						found = true
						break
					}
				}
				if !found &&
					!containsChange(changes, prev.NodeType) {
					changes = append(
						changes,
						prev.NodeType+" \u2192 Seq Scan",
					)
				}
			}
		}
	}
	return changes
}

func containsChange(changes []string, nodeType string) bool {
	for _, c := range changes {
		if strings.Contains(c, nodeType) {
			return true
		}
	}
	return false
}

// planNodeFull includes all fields needed for spill detection.
type planNodeFull struct {
	NodeType      string         `json:"Node Type"`
	RelationName  string         `json:"Relation Name"`
	SortSpaceType string         `json:"Sort Space Type"`
	HashBatches   int            `json:"Hash Batches"`
	Plans         []planNodeFull `json:"Plans"`
}

// hasDiskSpill returns true if the plan contains a Sort with disk
// spill or a Hash with batches > 1.
func hasDiskSpill(planJSON []byte) bool {
	var wrapper []struct {
		Plan planNodeFull `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &wrapper); err != nil {
		return false
	}
	if len(wrapper) == 0 {
		return false
	}
	return walkForSpill(wrapper[0].Plan)
}

func walkForSpill(node planNodeFull) bool {
	if node.NodeType == "Sort" && node.SortSpaceType == "Disk" {
		return true
	}
	if strings.Contains(node.NodeType, "Hash") &&
		node.HashBatches > 1 {
		return true
	}
	for _, child := range node.Plans {
		if walkForSpill(child) {
			return true
		}
	}
	return false
}

// severityFromCostRatio maps a cost ratio to a severity level.
func severityFromCostRatio(ratio float64) string {
	if ratio >= 10.0 {
		return "critical"
	}
	if ratio >= 2.0 {
		return "warning"
	}
	return "info"
}

// buildPlanSummary produces a compact one-line summary of a plan
// like "Seq Scan → Sort (Disk) → Limit".
func buildPlanSummary(planJSON []byte) string {
	root, ok := parsePlanRoot(planJSON)
	if !ok {
		return ""
	}
	var parts []string
	collectSummaryChain(root, planJSON, &parts)
	return strings.Join(parts, " \u2192 ")
}

func collectSummaryChain(
	node planNode, rawJSON []byte, parts *[]string,
) {
	label := node.NodeType
	if hasDiskSpillForNode(node.NodeType, rawJSON) {
		label += " (Disk)"
	}
	*parts = append(*parts, label)
	if len(node.Plans) > 0 {
		collectSummaryChain(node.Plans[0], rawJSON, parts)
	}
}

// hasDiskSpillForNode is a helper that checks if a specific Sort
// node in the raw JSON has Disk spill. This is a rough heuristic
// checking the raw JSON.
func hasDiskSpillForNode(nodeType string, rawJSON []byte) bool {
	if nodeType != "Sort" {
		return false
	}
	var wrapper []struct {
		Plan planNodeFull `json:"Plan"`
	}
	if err := json.Unmarshal(rawJSON, &wrapper); err != nil {
		return false
	}
	if len(wrapper) == 0 {
		return false
	}
	return findSortDisk(wrapper[0].Plan)
}

func findSortDisk(node planNodeFull) bool {
	if node.NodeType == "Sort" && node.SortSpaceType == "Disk" {
		return true
	}
	for _, child := range node.Plans {
		if findSortDisk(child) {
			return true
		}
	}
	return false
}

// checkPlanRegression loads the two most recent plans per query
// from explain_cache and runs plan regression detection.
func (a *Analyzer) checkPlanRegression(
	ctx context.Context,
) []Finding {
	rows, err := a.pool.Query(ctx, `
		WITH ranked AS (
			SELECT queryid, query_text, plan_json,
				total_cost, execution_time,
				ROW_NUMBER() OVER (
					PARTITION BY queryid
					ORDER BY captured_at DESC
				) AS rn
			FROM sage.explain_cache
			WHERE captured_at > now() - interval '7 days'
		)
		SELECT
			c.queryid, c.query_text,
			c.plan_json, c.total_cost, c.execution_time,
			p.plan_json, p.total_cost, p.execution_time
		FROM ranked c
		JOIN ranked p ON c.queryid = p.queryid AND p.rn = 2
		WHERE c.rn = 1`)
	if err != nil {
		a.logFn(
			"ERROR",
			"analyzer: plan_regression query: %v", err,
		)
		return nil
	}
	defer rows.Close()

	var pairs []planPair
	for rows.Next() {
		var p planPair
		if err := rows.Scan(
			&p.QueryID, &p.QueryText,
			&p.CurrentPlan, &p.CurrentCost, &p.CurrentTime,
			&p.PreviousPlan, &p.PreviousCost, &p.PreviousTime,
		); err != nil {
			a.logFn(
				"WARN",
				"analyzer: scan plan pair: %v", err,
			)
			continue
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		a.logFn(
			"ERROR",
			"analyzer: iterate plan pairs: %v", err,
		)
	}
	return rulePlanRegression(pairs)
}
