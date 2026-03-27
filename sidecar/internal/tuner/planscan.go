package tuner

import (
	"encoding/json"
	"fmt"
	"strings"
)

type planNode struct {
	NodeType            string     `json:"Node Type"`
	RelationName        string     `json:"Relation Name,omitempty"`
	Schema              string     `json:"Schema,omitempty"`
	Alias               string     `json:"Alias,omitempty"`
	JoinType            string     `json:"Join Type,omitempty"`
	IndexName           string     `json:"Index Name,omitempty"`
	PlanRows            int64      `json:"Plan Rows"`
	ActualRows          *int64     `json:"Actual Rows,omitempty"`
	ActualLoops         *int64     `json:"Actual Loops,omitempty"`
	SortMethod          *string    `json:"Sort Method,omitempty"`
	SortSpaceUsed       *int64     `json:"Sort Space Used,omitempty"`
	SortSpaceType       *string    `json:"Sort Space Type,omitempty"`
	HashBatches         *int64     `json:"Hash Batches,omitempty"`
	OriginalHashBatches *int64     `json:"Original Hash Batches,omitempty"`
	PeakMemoryUsage     *int64     `json:"Peak Memory Usage,omitempty"`
	Filter              *string    `json:"Filter,omitempty"`
	RowsRemovedByFilter *int64     `json:"Rows Removed by Filter,omitempty"`
	WorkersPlanned      *int       `json:"Workers Planned,omitempty"`
	WorkersLaunched     *int       `json:"Workers Launched,omitempty"`
	Plans               []planNode `json:"Plans,omitempty"`
}

type planWrapper struct {
	Plan planNode `json:"Plan"`
}

// ScanPlan parses EXPLAIN (FORMAT JSON) output and returns
// detected symptoms. Accepts both [{"Plan":...}] and {"Plan":...}.
func ScanPlan(planJSON []byte) ([]PlanSymptom, error) {
	root, err := parsePlanRoot(planJSON)
	if err != nil {
		return nil, fmt.Errorf("tuner: parse plan: %w", err)
	}
	var symptoms []PlanSymptom
	walkNode(root, 0, &symptoms)
	return symptoms, nil
}

func parsePlanRoot(data []byte) (planNode, error) {
	// Try array wrapper first: [{"Plan": ...}]
	var arr []planWrapper
	if err := json.Unmarshal(data, &arr); err == nil {
		if len(arr) == 0 {
			return planNode{}, nil
		}
		return arr[0].Plan, nil
	}
	// Try bare object: {"Plan": ...}
	var single planWrapper
	if err := json.Unmarshal(data, &single); err != nil {
		return planNode{}, fmt.Errorf("invalid plan JSON: %w", err)
	}
	return single.Plan, nil
}

func walkNode(
	node planNode, depth int, symptoms *[]PlanSymptom,
) {
	walkNodeWithParent(node, nil, depth, symptoms)
}

func walkNodeWithParent(
	node planNode,
	parent *planNode,
	depth int,
	symptoms *[]PlanSymptom,
) {
	found := checkNode(node, depth)
	*symptoms = append(*symptoms, found...)
	if s := checkSortLimit(node, parent, depth); s != nil {
		*symptoms = append(*symptoms, *s)
	}
	for i := range node.Plans {
		walkNodeWithParent(
			node.Plans[i], &node, depth+1, symptoms,
		)
	}
}

func checkNode(node planNode, depth int) []PlanSymptom {
	var out []PlanSymptom
	if s := checkDiskSort(node, depth); s != nil {
		out = append(out, *s)
	}
	if s := checkHashSpill(node, depth); s != nil {
		out = append(out, *s)
	}
	if s := checkBadNestedLoop(node, depth); s != nil {
		out = append(out, *s)
	}
	if s := checkSeqScan(node, depth); s != nil {
		out = append(out, *s)
	}
	if s := checkParallelDisabled(node, depth); s != nil {
		out = append(out, *s)
	}
	return out
}

func checkDiskSort(n planNode, depth int) *PlanSymptom {
	if n.SortSpaceType == nil || *n.SortSpaceType != "Disk" {
		return nil
	}
	var used int64
	if n.SortSpaceUsed != nil {
		used = *n.SortSpaceUsed
	}
	return &PlanSymptom{
		Kind:      SymptomDiskSort,
		NodeType:  n.NodeType,
		NodeDepth: depth,
		Detail:    map[string]any{"sort_space_kb": used},
	}
}

func checkHashSpill(n planNode, depth int) *PlanSymptom {
	if n.HashBatches == nil || *n.HashBatches <= 1 {
		return nil
	}
	detail := map[string]any{
		"hash_batches": *n.HashBatches,
	}
	if n.PeakMemoryUsage != nil {
		detail["peak_memory_kb"] = *n.PeakMemoryUsage
	}
	return &PlanSymptom{
		Kind:      SymptomHashSpill,
		NodeType:  n.NodeType,
		NodeDepth: depth,
		Detail:    detail,
	}
}

func checkBadNestedLoop(
	n planNode, depth int,
) *PlanSymptom {
	if n.NodeType != "Nested Loop" {
		return nil
	}
	if n.ActualRows == nil || n.PlanRows <= 0 {
		return nil
	}
	if *n.ActualRows <= n.PlanRows*10 {
		return nil
	}
	return &PlanSymptom{
		Kind:      SymptomBadNestedLoop,
		NodeType:  n.NodeType,
		NodeDepth: depth,
		Alias:     n.Alias,
		Detail: map[string]any{
			"plan_rows":   n.PlanRows,
			"actual_rows": *n.ActualRows,
		},
	}
}

func checkSeqScan(n planNode, depth int) *PlanSymptom {
	if n.NodeType != "Seq Scan" {
		return nil
	}
	if n.RelationName == "" {
		return nil
	}
	return &PlanSymptom{
		Kind:         SymptomSeqScanWithIndex,
		NodeType:     n.NodeType,
		NodeDepth:    depth,
		RelationName: n.RelationName,
		Schema:       n.Schema,
		Alias:        n.Alias,
	}
}

// checkSortLimit detects Sort nodes under a Limit parent where
// the sort processes 10x+ more rows than the limit needs.
func checkSortLimit(
	n planNode, parent *planNode, depth int,
) *PlanSymptom {
	if !strings.HasPrefix(n.NodeType, "Sort") {
		return nil
	}
	if parent == nil || parent.NodeType != "Limit" {
		return nil
	}
	if parent.PlanRows <= 0 || n.PlanRows <= 0 {
		return nil
	}
	if n.PlanRows < parent.PlanRows*10 {
		return nil
	}
	return &PlanSymptom{
		Kind:      SymptomSortLimit,
		NodeType:  n.NodeType,
		NodeDepth: depth,
		Detail: map[string]any{
			"sort_rows":  n.PlanRows,
			"limit_rows": parent.PlanRows,
		},
	}
}

func checkParallelDisabled(
	n planNode, depth int,
) *PlanSymptom {
	if !strings.Contains(n.NodeType, "Scan") {
		return nil
	}
	if n.RelationName == "" {
		return nil
	}
	if n.WorkersPlanned != nil {
		return nil
	}
	return &PlanSymptom{
		Kind:         SymptomParallelDisabled,
		NodeType:     n.NodeType,
		NodeDepth:    depth,
		RelationName: n.RelationName,
		Schema:       n.Schema,
		Alias:        n.Alias,
	}
}
