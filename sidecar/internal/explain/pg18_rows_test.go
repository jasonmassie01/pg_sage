package explain

import (
	"encoding/json"
	"strings"
	"testing"
)

// PG18 reports "Actual Rows" as a per-loop average with two decimals. Parsing
// it as an integer failed the whole plan, so the node breakdown was empty.
func TestExtractNodesAcceptsPG18FractionalActualRows(t *testing.T) {
	plan := json.RawMessage(`[{"Plan":{"Node Type":"Nested Loop","Total Cost":9.5,
		"Plan Rows":10,"Actual Rows":250.50,"Actual Total Time":1.25,
		"Plans":[{"Node Type":"Seq Scan","Relation Name":"orders","Total Cost":4,
		"Plan Rows":5,"Actual Rows":0.50}]}}]`)
	nodes := extractNodes(plan)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if nodes[0].Rows != 251 || nodes[1].Rows != 1 {
		t.Fatalf("rows = %d, %d; want 251, 1 (rounded half up)", nodes[0].Rows, nodes[1].Rows)
	}
	if !strings.Contains(nodes[0].Warning, "off by 25x") {
		t.Fatalf("estimate warning = %q, want the 25x skew", nodes[0].Warning)
	}
	if nodes[1].Relation != "orders" || nodes[1].Warning != "" {
		t.Fatalf("child node = %+v", nodes[1])
	}
}
