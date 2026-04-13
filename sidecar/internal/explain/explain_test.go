package explain

import (
	"encoding/json"
	"testing"
)

// ---------- isDDL ----------

func TestIsDDL(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"SELECT", "SELECT * FROM t", false},
		{"CREATE TABLE", "CREATE TABLE t (id int)", true},
		{"DROP INDEX", "DROP INDEX idx", true},
		{"ALTER TABLE", "ALTER TABLE t ADD COLUMN c text", true},
		{"TRUNCATE", "TRUNCATE t", true},
		{"GRANT", "GRANT SELECT ON t TO role", true},
		{"REINDEX", "REINDEX INDEX idx", true},
		{"leading whitespace", "  CREATE TABLE t (id int)", true},
		{"lowercase", "create table t (id int)", true},
		{"empty string", "", false},
		{"INSERT", "INSERT INTO t VALUES (1)", false},
		{"UPDATE", "UPDATE t SET c = 1", false},
		{"DELETE", "DELETE FROM t", false},
		{"COPY", "COPY t FROM '/tmp/data.csv'", true},
		{"CLUSTER", "CLUSTER t USING idx", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDDL(tt.query)
			if got != tt.want {
				t.Errorf("isDDL(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// ---------- hasParamPlaceholder ----------

func TestHasParamPlaceholder(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"single $1", "SELECT * FROM t WHERE id = $1", true},
		{"no placeholder", "SELECT * FROM t WHERE id = 1", false},
		{"multiple placeholders", "SELECT $1, $2", true},
		{"empty string", "", false},
		{
			"dollar sign inside string literal",
			"SELECT * FROM t WHERE name = 'has $1 in string'",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasParamPlaceholder(tt.query)
			if got != tt.want {
				t.Errorf(
					"hasParamPlaceholder(%q) = %v, want %v",
					tt.query, got, tt.want,
				)
			}
		})
	}
}

// ---------- normalizeQuery ----------

func TestNormalizeQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"collapse whitespace",
			"SELECT  *   FROM  t",
			"SELECT * FROM t",
		},
		{
			"strip line comment",
			"SELECT * FROM t -- comment\nWHERE id = 1",
			"SELECT * FROM t WHERE id = 1",
		},
		{
			"strip block comment",
			"SELECT /* block comment */ * FROM t",
			"SELECT * FROM t",
		},
		{
			"trim leading and trailing whitespace",
			"  SELECT * FROM t  ",
			"SELECT * FROM t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeQuery(tt.input)
			if got != tt.want {
				t.Errorf(
					"normalizeQuery(%q)\n  got  %q\n  want %q",
					tt.input, got, tt.want,
				)
			}
		})
	}
}

// ---------- queryHash ----------

func TestQueryHash(t *testing.T) {
	t.Run("same query same hash", func(t *testing.T) {
		h1 := queryHash("SELECT * FROM t", nil)
		h2 := queryHash("SELECT * FROM t", nil)
		if h1 != h2 {
			t.Errorf("identical queries produced different hashes: %d vs %d", h1, h2)
		}
	})

	t.Run("different queries different hashes", func(t *testing.T) {
		h1 := queryHash("SELECT * FROM t", nil)
		h2 := queryHash("SELECT * FROM other_table", nil)
		if h1 == h2 {
			t.Error("different queries produced the same hash")
		}
	})

	t.Run("same query different params different hashes", func(t *testing.T) {
		h1 := queryHash("SELECT * FROM t WHERE id = $1", []string{"1"})
		h2 := queryHash("SELECT * FROM t WHERE id = $1", []string{"2"})
		if h1 == h2 {
			t.Error("same query with different params produced the same hash")
		}
	})

	t.Run("comments stripped before hashing", func(t *testing.T) {
		h1 := queryHash("SELECT * FROM t", nil)
		h2 := queryHash("SELECT * FROM t -- with comment", nil)
		if h1 != h2 {
			t.Errorf(
				"query with stripped comment produced different hash: %d vs %d",
				h1, h2,
			)
		}
	})
}

// ---------- explainSQL ----------

func TestExplainSQL(t *testing.T) {
	t.Run("plan only", func(t *testing.T) {
		got := explainSQL("SELECT * FROM orders", false)
		want := "EXPLAIN (FORMAT JSON) SELECT * FROM orders"
		if got != want {
			t.Errorf("explainSQL(_, false)\n  got  %q\n  want %q", got, want)
		}
	})

	t.Run("analyze", func(t *testing.T) {
		got := explainSQL("SELECT * FROM orders", true)
		want := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT * FROM orders"
		if got != want {
			t.Errorf("explainSQL(_, true)\n  got  %q\n  want %q", got, want)
		}
	})
}

// ---------- describeNode ----------

func TestDescribeNode(t *testing.T) {
	tests := []struct {
		name     string
		nodeType string
		relation string
		want     string
	}{
		{"with relation", "Seq Scan", "orders", "Seq Scan on orders"},
		{"without relation", "Hash Join", "", "Hash Join"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeNode(tt.nodeType, tt.relation)
			if got != tt.want {
				t.Errorf(
					"describeNode(%q, %q) = %q, want %q",
					tt.nodeType, tt.relation, got, tt.want,
				)
			}
		})
	}
}

// ---------- extractNodes ----------

func TestExtractNodes(t *testing.T) {
	t.Run("nested plan tree flattened", func(t *testing.T) {
		planJSON := json.RawMessage(`[{
			"Plan": {
				"Node Type": "Hash Join",
				"Total Cost": 200.0,
				"Plan Rows": 100,
				"Plans": [
					{
						"Node Type": "Seq Scan",
						"Relation Name": "orders",
						"Total Cost": 100.0,
						"Plan Rows": 1000,
						"Actual Total Time": 12.5,
						"Actual Rows": 50000
					},
					{
						"Node Type": "Hash",
						"Total Cost": 50.0,
						"Plan Rows": 10,
						"Plans": [
							{
								"Node Type": "Index Scan",
								"Relation Name": "customers",
								"Total Cost": 25.0,
								"Plan Rows": 10
							}
						]
					}
				]
			}
		}]`)

		nodes := extractNodes(planJSON)
		if nodes == nil {
			t.Fatal("extractNodes returned nil for valid plan JSON")
		}
		if len(nodes) != 4 {
			t.Fatalf("expected 4 nodes, got %d", len(nodes))
		}

		// Root node
		if nodes[0].NodeType != "Hash Join" {
			t.Errorf("node[0].NodeType = %q, want %q", nodes[0].NodeType, "Hash Join")
		}
		if nodes[0].Relation != "" {
			t.Errorf("node[0].Relation = %q, want empty", nodes[0].Relation)
		}
		if nodes[0].Description != "Hash Join" {
			t.Errorf("node[0].Description = %q, want %q", nodes[0].Description, "Hash Join")
		}

		// First child: Seq Scan on orders
		if nodes[1].NodeType != "Seq Scan" {
			t.Errorf("node[1].NodeType = %q, want %q", nodes[1].NodeType, "Seq Scan")
		}
		if nodes[1].Relation != "orders" {
			t.Errorf("node[1].Relation = %q, want %q", nodes[1].Relation, "orders")
		}
		if nodes[1].Description != "Seq Scan on orders" {
			t.Errorf(
				"node[1].Description = %q, want %q",
				nodes[1].Description, "Seq Scan on orders",
			)
		}

		// Deepest child: Index Scan on customers
		if nodes[3].NodeType != "Index Scan" {
			t.Errorf("node[3].NodeType = %q, want %q", nodes[3].NodeType, "Index Scan")
		}
		if nodes[3].Relation != "customers" {
			t.Errorf("node[3].Relation = %q, want %q", nodes[3].Relation, "customers")
		}
	})

	t.Run("row estimate warning", func(t *testing.T) {
		planJSON := json.RawMessage(`[{
			"Plan": {
				"Node Type": "Seq Scan",
				"Relation Name": "orders",
				"Total Cost": 100.0,
				"Plan Rows": 1000,
				"Actual Total Time": 12.5,
				"Actual Rows": 50000
			}
		}]`)

		nodes := extractNodes(planJSON)
		if nodes == nil {
			t.Fatal("extractNodes returned nil")
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		if nodes[0].Warning == "" {
			t.Error("expected a row-estimate warning, got empty string")
		}
		if nodes[0].Rows != 50000 {
			t.Errorf("node.Rows = %d, want 50000", nodes[0].Rows)
		}
		if nodes[0].RowEstimate != 1000 {
			t.Errorf("node.RowEstimate = %d, want 1000", nodes[0].RowEstimate)
		}
		if nodes[0].TimeMs == nil {
			t.Error("expected TimeMs to be populated, got nil")
		} else if *nodes[0].TimeMs != 12.5 {
			t.Errorf("node.TimeMs = %f, want 12.5", *nodes[0].TimeMs)
		}
	})

	t.Run("empty JSON returns nil", func(t *testing.T) {
		nodes := extractNodes(json.RawMessage(`[]`))
		if nodes != nil {
			t.Errorf("expected nil for empty JSON array, got %v", nodes)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		nodes := extractNodes(json.RawMessage(`not json`))
		if nodes != nil {
			t.Errorf("expected nil for invalid JSON, got %v", nodes)
		}
	})

	t.Run("single node plan", func(t *testing.T) {
		planJSON := json.RawMessage(`[{
			"Plan": {
				"Node Type": "Seq Scan",
				"Relation Name": "users",
				"Total Cost": 42.0,
				"Plan Rows": 500
			}
		}]`)

		nodes := extractNodes(planJSON)
		if nodes == nil {
			t.Fatal("extractNodes returned nil for single-node plan")
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		if nodes[0].NodeType != "Seq Scan" {
			t.Errorf("node.NodeType = %q, want %q", nodes[0].NodeType, "Seq Scan")
		}
		if nodes[0].Relation != "users" {
			t.Errorf("node.Relation = %q, want %q", nodes[0].Relation, "users")
		}
		if nodes[0].RowEstimate != 500 {
			t.Errorf("node.RowEstimate = %d, want 500", nodes[0].RowEstimate)
		}
		// No ActualRows in JSON, so Rows should be zero-value.
		if nodes[0].Rows != 0 {
			t.Errorf("node.Rows = %d, want 0 (no actual rows)", nodes[0].Rows)
		}
		// No actual data, so no warning expected.
		if nodes[0].Warning != "" {
			t.Errorf("expected no warning, got %q", nodes[0].Warning)
		}
	})
}

// ---------- buildResult ----------

func TestBuildResult(t *testing.T) {
	// Realistic EXPLAIN ANALYZE JSON output.
	analyzedPlanJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "orders",
			"Total Cost": 150.5,
			"Plan Rows": 1000,
			"Actual Total Time": 8.3,
			"Actual Rows": 1000
		},
		"Planning Time": 0.45,
		"Execution Time": 8.72
	}]`)

	// Plan-only JSON (no Actual fields at the top-level wrapper, but
	// the Plan node still has Total Cost and Plan Rows).
	planOnlyJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Index Scan",
			"Relation Name": "users",
			"Total Cost": 42.0,
			"Plan Rows": 200
		}
	}]`)

	t.Run("analyzed result populates timing", func(t *testing.T) {
		result := buildResult("SELECT * FROM orders", analyzedPlanJSON, true)
		if result == nil {
			t.Fatal("buildResult returned nil")
		}
		if result.Query != "SELECT * FROM orders" {
			t.Errorf("Query = %q, want %q", result.Query, "SELECT * FROM orders")
		}
		if result.EstimatedCost != 150.5 {
			t.Errorf("EstimatedCost = %f, want 150.5", result.EstimatedCost)
		}
		if result.ActualTimeMs == nil {
			t.Fatal("ActualTimeMs is nil, expected populated")
		}
		if *result.ActualTimeMs != 8.3 {
			t.Errorf("ActualTimeMs = %f, want 8.3", *result.ActualTimeMs)
		}
		if result.PlanningTimeMs == nil {
			t.Fatal("PlanningTimeMs is nil, expected populated")
		}
		if *result.PlanningTimeMs != 0.45 {
			t.Errorf("PlanningTimeMs = %f, want 0.45", *result.PlanningTimeMs)
		}
		if result.Note != "" {
			t.Errorf("Note = %q, want empty for analyzed result", result.Note)
		}
	})

	t.Run("plan-only result sets note", func(t *testing.T) {
		result := buildResult("SELECT * FROM users WHERE id = $1", planOnlyJSON, false)
		if result == nil {
			t.Fatal("buildResult returned nil")
		}
		want := "EXPLAIN without ANALYZE (query has parameters)"
		if result.Note != want {
			t.Errorf("Note = %q, want %q", result.Note, want)
		}
		if result.ActualTimeMs != nil {
			t.Errorf(
				"ActualTimeMs = %f, want nil for plan-only",
				*result.ActualTimeMs,
			)
		}
	})

	t.Run("estimated cost extracted from plan", func(t *testing.T) {
		result := buildResult("SELECT 1", planOnlyJSON, false)
		if result.EstimatedCost != 42.0 {
			t.Errorf("EstimatedCost = %f, want 42.0", result.EstimatedCost)
		}
	})

	t.Run("node breakdown populated", func(t *testing.T) {
		result := buildResult("SELECT * FROM orders", analyzedPlanJSON, true)
		if len(result.NodeBreakdown) == 0 {
			t.Fatal("NodeBreakdown is empty")
		}
		if result.NodeBreakdown[0].NodeType != "Seq Scan" {
			t.Errorf(
				"NodeBreakdown[0].NodeType = %q, want %q",
				result.NodeBreakdown[0].NodeType, "Seq Scan",
			)
		}
		if result.NodeBreakdown[0].Relation != "orders" {
			t.Errorf(
				"NodeBreakdown[0].Relation = %q, want %q",
				result.NodeBreakdown[0].Relation, "orders",
			)
		}
	})

	t.Run("SlowBecause and Recommendations initialized as empty slices", func(t *testing.T) {
		result := buildResult("SELECT 1", planOnlyJSON, false)
		if result.SlowBecause == nil {
			t.Error("SlowBecause is nil, want non-nil empty slice")
		}
		if len(result.SlowBecause) != 0 {
			t.Errorf("SlowBecause length = %d, want 0", len(result.SlowBecause))
		}
		if result.Recommendations == nil {
			t.Error("Recommendations is nil, want non-nil empty slice")
		}
		if len(result.Recommendations) != 0 {
			t.Errorf(
				"Recommendations length = %d, want 0",
				len(result.Recommendations),
			)
		}
	})

	t.Run("Summary has default value", func(t *testing.T) {
		result := buildResult("SELECT 1", planOnlyJSON, false)
		if result.Summary == "" {
			t.Error("Summary is empty, expected default value")
		}
	})
}
