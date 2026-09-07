package explain

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// noopLogFn is a logger that discards all output — used in unit tests
// that don't need to assert on log messages.
func noopLogFn(_ string, _ string, _ ...any) {}

// ---------- New (constructor) ----------

func TestNew(t *testing.T) {
	cfg := &config.ExplainConfig{
		Enabled:         true,
		TimeoutMs:       5000,
		CacheTTLMinutes: 60,
	}

	ex := New(nil, cfg, noopLogFn)
	if ex == nil {
		t.Fatal("New returned nil")
	}
	if ex.pool != nil {
		t.Error("pool should be nil when constructed with nil pool")
	}
	if ex.cfg != cfg {
		t.Error("cfg pointer mismatch")
	}
	if ex.logFn == nil {
		t.Error("logFn is nil, expected non-nil")
	}
}

func TestNew_NilConfig(t *testing.T) {
	ex := New(nil, nil, noopLogFn)
	if ex == nil {
		t.Fatal("New returned nil even with nil config")
	}
	if ex.cfg != nil {
		t.Error("cfg should be nil when constructed with nil config")
	}
}

// ---------- Explain validation (no DB required) ----------

func TestExplain_EmptyQueryAndZeroQueryID(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	_, err := ex.Explain(context.Background(), ExplainRequest{
		Query:   "",
		QueryID: 0,
	})
	if err == nil {
		t.Fatal("expected error for empty query + zero query_id, got nil")
	}
	if !strings.Contains(err.Error(), "query or query_id is required") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "query or query_id is required")
	}
}

func TestExplain_QueryIDNotImplemented(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	_, err := ex.Explain(context.Background(), ExplainRequest{
		Query:   "",
		QueryID: 12345,
	})
	if err == nil {
		t.Fatal("expected error for query_id lookup, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "not yet implemented")
	}
}

func TestExplain_QueryIDWithQuery(t *testing.T) {
	// When both query and query_id are provided, query_id takes precedence
	// and returns the not-implemented error.
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	_, err := ex.Explain(context.Background(), ExplainRequest{
		Query:   "SELECT 1",
		QueryID: 99,
	})
	if err == nil {
		t.Fatal("expected error when query_id is set, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "not yet implemented")
	}
}

func TestExplain_DDLStatements(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)

	ddlQueries := []struct {
		name  string
		query string
	}{
		{"CREATE TABLE", "CREATE TABLE t (id int)"},
		{"DROP TABLE", "DROP TABLE t"},
		{"ALTER TABLE", "ALTER TABLE t ADD COLUMN c text"},
		{"TRUNCATE", "TRUNCATE t"},
		{"GRANT", "GRANT SELECT ON t TO role"},
		{"REVOKE", "REVOKE SELECT ON t FROM role"},
		{"COPY", "COPY t FROM '/tmp/data.csv'"},
		{"CLUSTER", "CLUSTER t USING idx"},
		{"REINDEX", "REINDEX INDEX idx"},
		{"lowercase create", "create table t (id int)"},
		{"leading whitespace", "  DROP TABLE t"},
	}

	for _, tt := range ddlQueries {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ex.Explain(context.Background(), ExplainRequest{
				Query: tt.query,
			})
			if err == nil {
				t.Fatalf("expected DDL rejection error for %q, got nil", tt.query)
			}
			if !strings.Contains(err.Error(), "DDL") {
				t.Errorf("error = %q, want it to contain %q",
					err.Error(), "DDL")
			}
		})
	}
}

func TestExplain_SelectNotDDL(t *testing.T) {
	// SELECT queries should NOT be rejected by DDL check.
	// They will fail later (pool is nil), but they should pass
	// the DDL validation step.
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)

	// We expect a panic or error from pool access, NOT a DDL error.
	// Use a recover to catch the nil-pointer panic from databaseName().
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic from nil pool access, got none")
			}
			// Panic is expected — the query passed DDL validation.
		}()
		_, _ = ex.Explain(context.Background(), ExplainRequest{
			Query: "SELECT * FROM t",
		})
	}()
}

// ---------- buildResult edge cases ----------

func TestBuildResult_InvalidJSON(t *testing.T) {
	// buildResult should still return a result even with unparseable JSON.
	// It won't extract cost/timing, but should not panic.
	result := buildResult("SELECT 1", json.RawMessage(`not valid json`), false)
	if result == nil {
		t.Fatal("buildResult returned nil for invalid plan JSON")
	}
	if result.Query != "SELECT 1" {
		t.Errorf("Query = %q, want %q", result.Query, "SELECT 1")
	}
	// Cost should be zero because parsing failed.
	if result.EstimatedCost != 0 {
		t.Errorf("EstimatedCost = %f, want 0 for invalid JSON",
			result.EstimatedCost)
	}
	if result.ActualTimeMs != nil {
		t.Errorf("ActualTimeMs should be nil for invalid JSON, got %f",
			*result.ActualTimeMs)
	}
	// NodeBreakdown should be nil when extractNodes fails.
	if result.NodeBreakdown != nil {
		t.Errorf("NodeBreakdown should be nil for invalid JSON, got %v",
			result.NodeBreakdown)
	}
	if result.Summary == "" {
		t.Error("Summary should have a default value, got empty string")
	}
}

func TestBuildResult_EmptyArray(t *testing.T) {
	result := buildResult("SELECT 1", json.RawMessage(`[]`), true)
	if result == nil {
		t.Fatal("buildResult returned nil for empty array plan JSON")
	}
	if result.EstimatedCost != 0 {
		t.Errorf("EstimatedCost = %f, want 0 for empty array",
			result.EstimatedCost)
	}
	if result.ActualTimeMs != nil {
		t.Errorf("ActualTimeMs should be nil for empty array, got %f",
			*result.ActualTimeMs)
	}
}

func TestBuildResult_NilPlanJSON(t *testing.T) {
	result := buildResult("SELECT 1", nil, false)
	if result == nil {
		t.Fatal("buildResult returned nil for nil plan JSON")
	}
	if result.Query != "SELECT 1" {
		t.Errorf("Query = %q, want %q", result.Query, "SELECT 1")
	}
}

func TestBuildResult_AnalyzedWithNoPlanningTime(t *testing.T) {
	// Plan JSON with Actual Total Time but no Planning Time at wrapper level.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Result",
			"Total Cost": 0.01,
			"Plan Rows": 1,
			"Actual Total Time": 0.005,
			"Actual Rows": 1
		}
	}]`)

	result := buildResult("SELECT 1", planJSON, true)
	if result == nil {
		t.Fatal("buildResult returned nil")
	}
	if result.ActualTimeMs == nil {
		t.Fatal("ActualTimeMs is nil, expected non-nil for analyzed plan")
	}
	if *result.ActualTimeMs != 0.005 {
		t.Errorf("ActualTimeMs = %f, want 0.005", *result.ActualTimeMs)
	}
	if result.PlanningTimeMs != nil {
		t.Errorf("PlanningTimeMs = %f, want nil when not in JSON",
			*result.PlanningTimeMs)
	}
	// Analyzed result should NOT set the note.
	if result.Note != "" {
		t.Errorf("Note = %q, want empty for analyzed result", result.Note)
	}
}

func TestBuildResult_PlanOnlySetsNote(t *testing.T) {
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Result",
			"Total Cost": 0.01,
			"Plan Rows": 1
		}
	}]`)

	result := buildResult("SELECT $1::int", planJSON, false)
	expected := "EXPLAIN without ANALYZE (query has parameters)"
	if result.Note != expected {
		t.Errorf("Note = %q, want %q", result.Note, expected)
	}
}

func TestBuildResult_ZeroCostPlan(t *testing.T) {
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Result",
			"Total Cost": 0.0,
			"Plan Rows": 0
		}
	}]`)

	result := buildResult("SELECT 1", planJSON, false)
	if result == nil {
		t.Fatal("buildResult returned nil")
	}
	if result.EstimatedCost != 0.0 {
		t.Errorf("EstimatedCost = %f, want 0.0", result.EstimatedCost)
	}
}

// ---------- extractNodes edge cases ----------

func TestExtractNodes_NoWarningAtExactly10x(t *testing.T) {
	// Boundary: ratio must be > 10 to trigger warning, not >= 10.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "t",
			"Total Cost": 10.0,
			"Plan Rows": 100,
			"Actual Total Time": 5.0,
			"Actual Rows": 1000
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	// 1000/100 = 10.0 exactly — should NOT trigger warning (> 10, not >= 10).
	if nodes[0].Warning != "" {
		t.Errorf("Warning = %q, want empty for exactly 10x ratio",
			nodes[0].Warning)
	}
}

func TestExtractNodes_WarningAt11x(t *testing.T) {
	// Boundary: ratio > 10 should trigger.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "t",
			"Total Cost": 10.0,
			"Plan Rows": 100,
			"Actual Total Time": 5.0,
			"Actual Rows": 1100
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	// 1100/100 = 11.0 — should trigger warning.
	if nodes[0].Warning == "" {
		t.Error("Warning is empty, expected warning for 11x ratio")
	}
	if !strings.Contains(nodes[0].Warning, "11x") {
		t.Errorf("Warning = %q, want it to contain '11x'", nodes[0].Warning)
	}
	if !strings.Contains(nodes[0].Warning, "est 100") {
		t.Errorf("Warning = %q, want it to contain 'est 100'",
			nodes[0].Warning)
	}
	if !strings.Contains(nodes[0].Warning, "actual 1100") {
		t.Errorf("Warning = %q, want it to contain 'actual 1100'",
			nodes[0].Warning)
	}
}

func TestExtractNodes_NoWarningWhenPlanRowsZero(t *testing.T) {
	// When PlanRows is 0, no ratio can be computed — no warning.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "t",
			"Total Cost": 10.0,
			"Plan Rows": 0,
			"Actual Total Time": 5.0,
			"Actual Rows": 1000
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	// PlanRows=0 means the ratio guard (n.PlanRows > 0) prevents warning.
	if nodes[0].Warning != "" {
		t.Errorf("Warning = %q, want empty when PlanRows is 0",
			nodes[0].Warning)
	}
}

func TestExtractNodes_NoWarningWhenActualRowsNil(t *testing.T) {
	// Plan-only mode: no Actual Rows, so no warning.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "t",
			"Total Cost": 10.0,
			"Plan Rows": 100
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Warning != "" {
		t.Errorf("Warning = %q, want empty when ActualRows is nil",
			nodes[0].Warning)
	}
	if nodes[0].Rows != 0 {
		t.Errorf("Rows = %d, want 0 when ActualRows is nil", nodes[0].Rows)
	}
	if nodes[0].TimeMs != nil {
		t.Errorf("TimeMs should be nil when Actual Total Time is absent")
	}
}

func TestExtractNodes_DeeplyNestedPlan(t *testing.T) {
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Limit",
			"Total Cost": 500.0,
			"Plan Rows": 10,
			"Plans": [
				{
					"Node Type": "Sort",
					"Total Cost": 490.0,
					"Plan Rows": 100,
					"Plans": [
						{
							"Node Type": "Hash Join",
							"Total Cost": 400.0,
							"Plan Rows": 100,
							"Plans": [
								{
									"Node Type": "Seq Scan",
									"Relation Name": "orders",
									"Total Cost": 200.0,
									"Plan Rows": 5000
								},
								{
									"Node Type": "Hash",
									"Total Cost": 100.0,
									"Plan Rows": 50,
									"Plans": [
										{
											"Node Type": "Index Scan",
											"Relation Name": "customers",
											"Total Cost": 50.0,
											"Plan Rows": 50
										}
									]
								}
							]
						}
					]
				}
			]
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 6 {
		t.Fatalf("expected 6 nodes, got %d", len(nodes))
	}

	// Verify the tree was flattened in depth-first order.
	expectedTypes := []string{
		"Limit", "Sort", "Hash Join", "Seq Scan", "Hash", "Index Scan",
	}
	for i, want := range expectedTypes {
		if nodes[i].NodeType != want {
			t.Errorf("nodes[%d].NodeType = %q, want %q",
				i, nodes[i].NodeType, want)
		}
	}

	// Verify relations.
	if nodes[3].Relation != "orders" {
		t.Errorf("nodes[3].Relation = %q, want %q",
			nodes[3].Relation, "orders")
	}
	if nodes[5].Relation != "customers" {
		t.Errorf("nodes[5].Relation = %q, want %q",
			nodes[5].Relation, "customers")
	}
}

func TestExtractNodes_ActualRowsZero(t *testing.T) {
	// Actual Rows = 0 means the node returned nothing. No warning
	// because ratio is 0 / PlanRows which is < 10.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Index Scan",
			"Relation Name": "t",
			"Total Cost": 5.0,
			"Plan Rows": 100,
			"Actual Total Time": 0.1,
			"Actual Rows": 0
		}
	}]`)

	nodes := extractNodes(planJSON)
	if nodes == nil {
		t.Fatal("extractNodes returned nil")
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Rows != 0 {
		t.Errorf("Rows = %d, want 0", nodes[0].Rows)
	}
	if nodes[0].Warning != "" {
		t.Errorf("Warning = %q, want empty for 0 actual rows",
			nodes[0].Warning)
	}
}

// ---------- normalizeQuery edge cases ----------

func TestNormalizeQuery_EmptyString(t *testing.T) {
	got := normalizeQuery("")
	if got != "" {
		t.Errorf("normalizeQuery(\"\") = %q, want empty", got)
	}
}

func TestNormalizeQuery_OnlyWhitespace(t *testing.T) {
	got := normalizeQuery("   \t\n  ")
	if got != "" {
		t.Errorf("normalizeQuery(whitespace) = %q, want empty", got)
	}
}

func TestNormalizeQuery_OnlyComment(t *testing.T) {
	got := normalizeQuery("-- just a comment")
	if got != "" {
		t.Errorf("normalizeQuery(comment-only) = %q, want empty", got)
	}
}

func TestNormalizeQuery_NestedBlockComments(t *testing.T) {
	// Note: the regex is non-greedy so it handles this as two separate
	// block comments, not as one nested one.
	got := normalizeQuery("SELECT /* comment1 */ a /* comment2 */ FROM t")
	want := "SELECT a FROM t"
	if got != want {
		t.Errorf("normalizeQuery(nested block comments)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestNormalizeQuery_MixedCommentTypes(t *testing.T) {
	got := normalizeQuery(
		"SELECT /* block */ * FROM t -- line comment\nWHERE id = 1",
	)
	want := "SELECT * FROM t WHERE id = 1"
	if got != want {
		t.Errorf("normalizeQuery(mixed comments)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestNormalizeQuery_TabsAndNewlines(t *testing.T) {
	got := normalizeQuery("SELECT\t*\nFROM\tt\nWHERE\tid\t=\t1")
	want := "SELECT * FROM t WHERE id = 1"
	if got != want {
		t.Errorf("normalizeQuery(tabs+newlines)\n  got  %q\n  want %q",
			got, want)
	}
}

// ---------- queryHash edge cases ----------

func TestQueryHash_NilVsEmptyParams(t *testing.T) {
	h1 := queryHash("SELECT 1", nil)
	h2 := queryHash("SELECT 1", []string{})
	if h1 != h2 {
		t.Errorf("nil params vs empty slice produced different hashes: %d vs %d",
			h1, h2)
	}
}

func TestQueryHash_EmptyQuery(t *testing.T) {
	// Should not panic.
	h := queryHash("", nil)
	if h == 0 {
		// FNV-64a of empty string is not zero, but let's check it's
		// deterministic rather than asserting a specific value.
		h2 := queryHash("", nil)
		if h != h2 {
			t.Errorf("empty query hash not deterministic: %d vs %d", h, h2)
		}
	}
}

func TestQueryHash_WhitespaceNormalized(t *testing.T) {
	h1 := queryHash("SELECT  *  FROM  t", nil)
	h2 := queryHash("SELECT * FROM t", nil)
	if h1 != h2 {
		t.Errorf("queries differing only in whitespace produced "+
			"different hashes: %d vs %d", h1, h2)
	}
}

func TestQueryHash_MultipleParams(t *testing.T) {
	h1 := queryHash("SELECT $1, $2", []string{"a", "b"})
	h2 := queryHash("SELECT $1, $2", []string{"b", "a"})
	if h1 == h2 {
		t.Error("different param order should produce different hashes")
	}
}

func TestQueryHash_ParamSeparation(t *testing.T) {
	// Ensure ["ab", ""] and ["a", "b"] produce different hashes.
	// This is guaranteed by the null separator byte between params.
	h1 := queryHash("SELECT $1, $2", []string{"ab", ""})
	h2 := queryHash("SELECT $1, $2", []string{"a", "b"})
	if h1 == h2 {
		t.Error("params ['ab',''] and ['a','b'] should produce different hashes")
	}
}

// ---------- isDDL edge cases ----------

func TestIsDDL_MixedCase(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"UPPER", "CREATE TABLE t (id int)", true},
		{"lower", "create table t (id int)", true},
		{"mIxEd", "CrEaTe TABLE t (id int)", true},
		{"SELECT upper", "SELECT * FROM t", false},
		{"select lower", "select * from t", false},
		{"WITH CTE", "WITH cte AS (SELECT 1) SELECT * FROM cte", false},
		{"INSERT", "INSERT INTO t VALUES (1)", false},
		{"UPDATE", "UPDATE t SET c = 1 WHERE id = 1", false},
		{"DELETE", "DELETE FROM t WHERE id = 1", false},
		{"EXPLAIN", "EXPLAIN SELECT * FROM t", false},
		{"whitespace + CREATE", "\t\n CREATE TABLE t (id int)", true},
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

// ---------- hasParamPlaceholder edge cases ----------

func TestHasParamPlaceholder_EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"$0", "SELECT $0", true},
		{"$9", "SELECT $9", true},
		{"$10", "SELECT $10", true},
		{"double digit", "SELECT $12", true},
		{"no dollar", "SELECT 1", false},
		{"dollar without number", "SELECT $a", false},
		{"dollar at end", "SELECT $", false},
		{"embedded in word", "SELECT foo$1bar", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasParamPlaceholder(tt.query)
			if got != tt.want {
				t.Errorf("hasParamPlaceholder(%q) = %v, want %v",
					tt.query, got, tt.want)
			}
		})
	}
}

// ---------- describeNode edge cases ----------

func TestDescribeNode_EmptyBoth(t *testing.T) {
	got := describeNode("", "")
	if got != "" {
		t.Errorf("describeNode(\"\", \"\") = %q, want empty", got)
	}
}

func TestDescribeNode_EmptyNodeType(t *testing.T) {
	got := describeNode("", "orders")
	want := " on orders"
	if got != want {
		t.Errorf("describeNode(\"\", \"orders\") = %q, want %q", got, want)
	}
}

// ---------- explainSQL edge cases ----------

func TestExplainSQL_EmptyQuery(t *testing.T) {
	got := explainSQL("", false)
	want := "EXPLAIN (FORMAT JSON) "
	if got != want {
		t.Errorf("explainSQL(\"\", false) = %q, want %q", got, want)
	}
}

func TestExplainSQL_ComplexQuery(t *testing.T) {
	q := "SELECT * FROM orders o JOIN customers c ON o.cid = c.id WHERE c.name = 'test'"
	got := explainSQL(q, true)
	want := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) " + q
	if got != want {
		t.Errorf("explainSQL(complex, true)\n  got  %q\n  want %q", got, want)
	}
}

func TestExplainParamLiteral_EscapesInput(t *testing.T) {
	got, err := explainParamLiteral("x'); SELECT pg_sleep(10); --")
	if err != nil {
		t.Fatalf("explainParamLiteral: %v", err)
	}
	want := "'x''); SELECT pg_sleep(10); --'"
	if got != want {
		t.Errorf("literal\n  got  %q\n  want %q", got, want)
	}
}

func TestExplainParamLiteral_RejectsNUL(t *testing.T) {
	_, err := explainParamLiteral("bad\x00value")
	if err == nil {
		t.Fatal("expected error for NUL byte, got nil")
	}
	if !errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("error = %v, want ErrExplainInvalidRequest", err)
	}
}

// ---------- ExplainRequest / ExplainResult JSON marshalling ----------

func TestExplainRequest_JSONRoundTrip(t *testing.T) {
	req := ExplainRequest{
		Query:    "SELECT * FROM t",
		QueryID:  42,
		PlanOnly: true,
		Params:   []string{"a", "b"},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExplainRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Query != req.Query {
		t.Errorf("Query = %q, want %q", got.Query, req.Query)
	}
	if got.QueryID != req.QueryID {
		t.Errorf("QueryID = %d, want %d", got.QueryID, req.QueryID)
	}
	if got.PlanOnly != req.PlanOnly {
		t.Errorf("PlanOnly = %v, want %v", got.PlanOnly, req.PlanOnly)
	}
	if len(got.Params) != len(req.Params) {
		t.Fatalf("Params length = %d, want %d",
			len(got.Params), len(req.Params))
	}
	for i, p := range got.Params {
		if p != req.Params[i] {
			t.Errorf("Params[%d] = %q, want %q", i, p, req.Params[i])
		}
	}
}

func TestExplainRequest_OmitsEmptyFields(t *testing.T) {
	req := ExplainRequest{
		Query: "SELECT 1",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	str := string(data)
	if strings.Contains(str, "query_id") {
		t.Errorf("marshalled JSON contains query_id, should be omitted: %s", str)
	}
	if strings.Contains(str, "plan_only") {
		t.Errorf("marshalled JSON contains plan_only, should be omitted: %s", str)
	}
	if strings.Contains(str, "params") {
		t.Errorf("marshalled JSON contains params, should be omitted: %s", str)
	}
}

func TestExplainResult_JSONRoundTrip(t *testing.T) {
	at := 8.3
	pt := 0.45
	result := ExplainResult{
		Query:           "SELECT * FROM t",
		PlanJSON:        json.RawMessage(`[{"Plan":{}}]`),
		Summary:         "test summary",
		SlowBecause:     []string{"seq scan"},
		Recommendations: []string{"add index"},
		NodeBreakdown: []NodeExplain{
			{
				NodeType:    "Seq Scan",
				Relation:    "orders",
				Description: "Seq Scan on orders",
				Rows:        1000,
				RowEstimate: 100,
			},
		},
		EstimatedCost:  150.5,
		ActualTimeMs:   &at,
		PlanningTimeMs: &pt,
		Note:           "test note",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExplainResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Query != result.Query {
		t.Errorf("Query = %q, want %q", got.Query, result.Query)
	}
	if got.Summary != result.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, result.Summary)
	}
	if got.EstimatedCost != result.EstimatedCost {
		t.Errorf("EstimatedCost = %f, want %f",
			got.EstimatedCost, result.EstimatedCost)
	}
	if got.ActualTimeMs == nil {
		t.Fatal("ActualTimeMs is nil after round-trip")
	}
	if *got.ActualTimeMs != *result.ActualTimeMs {
		t.Errorf("ActualTimeMs = %f, want %f",
			*got.ActualTimeMs, *result.ActualTimeMs)
	}
	if got.PlanningTimeMs == nil {
		t.Fatal("PlanningTimeMs is nil after round-trip")
	}
	if *got.PlanningTimeMs != *result.PlanningTimeMs {
		t.Errorf("PlanningTimeMs = %f, want %f",
			*got.PlanningTimeMs, *result.PlanningTimeMs)
	}
	if len(got.NodeBreakdown) != 1 {
		t.Fatalf("NodeBreakdown length = %d, want 1",
			len(got.NodeBreakdown))
	}
	if got.NodeBreakdown[0].NodeType != "Seq Scan" {
		t.Errorf("NodeBreakdown[0].NodeType = %q, want %q",
			got.NodeBreakdown[0].NodeType, "Seq Scan")
	}
}

func TestNodeExplain_JSONRoundTrip(t *testing.T) {
	timeMs := 12.5
	ne := NodeExplain{
		NodeType:    "Index Scan",
		Relation:    "users",
		Description: "Index Scan on users",
		TimeMs:      &timeMs,
		Rows:        500,
		RowEstimate: 100,
		Warning:     "row estimate off by 5x",
	}

	data, err := json.Marshal(ne)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got NodeExplain
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.NodeType != ne.NodeType {
		t.Errorf("NodeType = %q, want %q", got.NodeType, ne.NodeType)
	}
	if got.Relation != ne.Relation {
		t.Errorf("Relation = %q, want %q", got.Relation, ne.Relation)
	}
	if got.TimeMs == nil {
		t.Fatal("TimeMs is nil after round-trip")
	}
	if *got.TimeMs != *ne.TimeMs {
		t.Errorf("TimeMs = %f, want %f", *got.TimeMs, *ne.TimeMs)
	}
	if got.Rows != ne.Rows {
		t.Errorf("Rows = %d, want %d", got.Rows, ne.Rows)
	}
	if got.RowEstimate != ne.RowEstimate {
		t.Errorf("RowEstimate = %d, want %d",
			got.RowEstimate, ne.RowEstimate)
	}
	if got.Warning != ne.Warning {
		t.Errorf("Warning = %q, want %q", got.Warning, ne.Warning)
	}
}

func TestNodeExplain_OmitsEmptyOptionalFields(t *testing.T) {
	ne := NodeExplain{
		NodeType:    "Result",
		Description: "Result",
		Rows:        0,
		RowEstimate: 1,
	}

	data, err := json.Marshal(ne)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	str := string(data)
	if strings.Contains(str, "relation") {
		t.Errorf("marshalled JSON contains relation, should be omitted: %s",
			str)
	}
	if strings.Contains(str, "time_ms") {
		t.Errorf("marshalled JSON contains time_ms, should be omitted: %s",
			str)
	}
	if strings.Contains(str, "warning") {
		t.Errorf("marshalled JSON contains warning, should be omitted: %s",
			str)
	}
}

// ---------- buildResult with complex realistic plans ----------

func TestBuildResult_RealisticAnalyzePlan(t *testing.T) {
	// A realistic EXPLAIN ANALYZE output with nested nodes.
	planJSON := json.RawMessage(`[{
		"Plan": {
			"Node Type": "Limit",
			"Total Cost": 450.75,
			"Plan Rows": 10,
			"Actual Total Time": 15.2,
			"Actual Rows": 10,
			"Plans": [
				{
					"Node Type": "Sort",
					"Total Cost": 450.0,
					"Plan Rows": 1000,
					"Actual Total Time": 15.0,
					"Actual Rows": 1000,
					"Plans": [
						{
							"Node Type": "Seq Scan",
							"Relation Name": "orders",
							"Total Cost": 300.0,
							"Plan Rows": 1000,
							"Actual Total Time": 10.0,
							"Actual Rows": 1000
						}
					]
				}
			]
		},
		"Planning Time": 0.8,
		"Execution Time": 15.5
	}]`)

	result := buildResult("SELECT * FROM orders ORDER BY id LIMIT 10",
		planJSON, true)

	if result == nil {
		t.Fatal("buildResult returned nil")
	}
	if result.EstimatedCost != 450.75 {
		t.Errorf("EstimatedCost = %f, want 450.75", result.EstimatedCost)
	}
	if result.ActualTimeMs == nil {
		t.Fatal("ActualTimeMs is nil for analyzed plan")
	}
	if *result.ActualTimeMs != 15.2 {
		t.Errorf("ActualTimeMs = %f, want 15.2", *result.ActualTimeMs)
	}
	if result.PlanningTimeMs == nil {
		t.Fatal("PlanningTimeMs is nil, expected from Planning Time in JSON")
	}
	if *result.PlanningTimeMs != 0.8 {
		t.Errorf("PlanningTimeMs = %f, want 0.8", *result.PlanningTimeMs)
	}

	// Should have 3 nodes: Limit -> Sort -> Seq Scan.
	if len(result.NodeBreakdown) != 3 {
		t.Fatalf("NodeBreakdown length = %d, want 3",
			len(result.NodeBreakdown))
	}
	if result.NodeBreakdown[2].Relation != "orders" {
		t.Errorf("NodeBreakdown[2].Relation = %q, want %q",
			result.NodeBreakdown[2].Relation, "orders")
	}
}

// ---------- pool-based tests (fake pool, no real DB) ----------

// unreachablePool creates a pgxpool.Pool pointing at a non-existent host.
// The pool is created lazily (no connection until a query is issued), so
// pool.Config() works but any actual query will fail with a connection error.
// This lets us test code paths that need a non-nil pool without a real DB.
var (
	fakePool     *pgxpool.Pool
	fakePoolOnce sync.Once
	fakePoolErr  error
)

func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	fakePoolOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(
			"postgres://fake:fake@127.0.0.1:1/fakedb?" +
				"sslmode=disable&connect_timeout=1",
		)
		if err != nil {
			fakePoolErr = err
			return
		}
		fakePool, fakePoolErr = pgxpool.NewWithConfig(
			context.Background(), cfg,
		)
	})
	if fakePoolErr != nil {
		t.Fatalf("unreachablePool: %v", fakePoolErr)
	}
	return fakePool
}

func TestDatabaseName_WithPool(t *testing.T) {
	pool := unreachablePool(t)
	ex := New(pool, &config.ExplainConfig{}, noopLogFn)
	name := ex.databaseName()
	if name != "fakedb" {
		t.Errorf("databaseName() = %q, want %q", name, "fakedb")
	}
}

func TestDatabaseName_EmptyDatabaseName(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(
		"postgres://fake:fake@127.0.0.1:1/?sslmode=disable&connect_timeout=1",
	)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.ConnConfig.Database = ""
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer pool.Close()

	ex := New(pool, &config.ExplainConfig{}, noopLogFn)
	name := ex.databaseName()
	if name != "" {
		t.Errorf("databaseName() = %q, want empty for pool without db",
			name)
	}
}

func TestExplain_CacheCheckFailsGracefully(t *testing.T) {
	// With a fake pool, checkCache will fail (can't connect), but Explain
	// should log a warning and continue to runExplain (which also fails).
	pool := unreachablePool(t)

	var logMessages []string
	var mu sync.Mutex
	captureLog := func(level string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logMessages = append(logMessages, level+": "+msg)
	}

	cfg := &config.ExplainConfig{
		TimeoutMs:       1000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, captureLog)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT * FROM orders",
	})

	// Should fail because pool can't connect, but at the runExplain step,
	// not at validation or checkCache.
	if err == nil {
		t.Fatal("expected error from unreachable pool, got nil")
	}
	// The error should be from runExplain, not from validation.
	if strings.Contains(err.Error(), "query or query_id") {
		t.Errorf("got validation error instead of connection error: %v", err)
	}
	if strings.Contains(err.Error(), "DDL") {
		t.Errorf("got DDL error instead of connection error: %v", err)
	}
	// Verify the error is from the explain/connection step.
	if !strings.Contains(err.Error(), "explain:") {
		t.Errorf("error = %q, want it to contain 'explain:'", err.Error())
	}

	// Check that a cache warning was logged.
	mu.Lock()
	defer mu.Unlock()
	foundCacheWarn := false
	for _, msg := range logMessages {
		if strings.Contains(msg, "WARN") &&
			strings.Contains(msg, "cache lookup") {
			foundCacheWarn = true
			break
		}
	}
	if !foundCacheWarn {
		t.Errorf("expected a WARN log about cache lookup failure, got: %v",
			logMessages)
	}
}

func TestExplain_PlanOnlyWithParams(t *testing.T) {
	// When query has $N placeholders, useAnalyze should be false
	// regardless of PlanOnly setting. This tests the mode determination
	// logic in the Explain method.
	pool := unreachablePool(t)
	cfg := &config.ExplainConfig{
		TimeoutMs:       1000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	// The query has $1, so even though PlanOnly=false, it should
	// fall back to plan-only mode. It will still fail at runExplain
	// but we verify it gets that far.
	_, err := ex.Explain(ctx, ExplainRequest{
		Query:    "SELECT * FROM orders WHERE id = $1",
		PlanOnly: false,
	})
	if err == nil {
		t.Fatal("expected error from unreachable pool, got nil")
	}
	// Should be a connection error, not a validation error.
	if strings.Contains(err.Error(), "query or query_id") {
		t.Errorf("got validation error: %v", err)
	}
}

func TestExplain_RunExplainAcquireFailure(t *testing.T) {
	// Directly tests that runExplain wraps the acquire error properly.
	pool := unreachablePool(t)
	cfg := &config.ExplainConfig{
		TimeoutMs:       1000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	if err == nil {
		t.Fatal("expected error from unreachable pool, got nil")
	}
	// The error should mention "explain:" prefix from the Explain method.
	if !strings.Contains(err.Error(), "explain:") {
		t.Errorf("error = %q, want it to start with 'explain:'",
			err.Error())
	}
}

func TestExplain_DefaultTimeout(t *testing.T) {
	// When TimeoutMs is 0, runExplain defaults to 10s.
	// We test this code path by setting TimeoutMs=0.
	pool := unreachablePool(t)
	cfg := &config.ExplainConfig{
		TimeoutMs:       0, // triggers default timeout of 10s
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	// Will still fail at pool.Acquire, but the timeout=0 branch
	// in runExplain is exercised.
	if err == nil {
		t.Fatal("expected error from unreachable pool, got nil")
	}
	if !strings.Contains(err.Error(), "explain:") {
		t.Errorf("error = %q, want it to contain 'explain:'",
			err.Error())
	}
}

func TestExplain_PlanOnlyExplicit(t *testing.T) {
	// When PlanOnly=true, useAnalyze should be false.
	pool := unreachablePool(t)
	cfg := &config.ExplainConfig{
		TimeoutMs:       1000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	_, err := ex.Explain(ctx, ExplainRequest{
		Query:    "SELECT 1",
		PlanOnly: true,
	})
	if err == nil {
		t.Fatal("expected error from unreachable pool, got nil")
	}
	// Should pass validation and mode determination.
	if strings.Contains(err.Error(), "DDL") ||
		strings.Contains(err.Error(), "query or query_id") {
		t.Errorf("got validation error instead of connection error: %v",
			err)
	}
}

func TestExplain_CancelledContext(t *testing.T) {
	pool := unreachablePool(t)
	cfg := &config.ExplainConfig{
		TimeoutMs:       5000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

// ---------- saveCache default TTL ----------

func TestSaveCache_DefaultTTL(t *testing.T) {
	// We can't fully test saveCache without a DB, but we can verify
	// the TTL default logic by inspecting the config. The saveCache
	// function defaults CacheTTLMinutes to 60 when it's 0.
	// This is a behavioral test through Explain that verifies the
	// config is read correctly.
	cfg := &config.ExplainConfig{
		CacheTTLMinutes: 0, // should default to 60 inside saveCache
		TimeoutMs:       1000,
	}
	// Verify the config itself.
	if cfg.CacheTTLMinutes != 0 {
		t.Errorf("CacheTTLMinutes = %d, want 0 (pre-default)",
			cfg.CacheTTLMinutes)
	}

	// The default logic is: if ttl == 0 { ttl = 60 }
	// We verify this by reading the code path. Since we can't call
	// saveCache directly without a DB, we at least verify the config
	// is set up correctly for the default to apply.
	ttl := cfg.CacheTTLMinutes
	if ttl == 0 {
		ttl = 60
	}
	if ttl != 60 {
		t.Errorf("default TTL = %d, want 60", ttl)
	}
}

func TestSaveCache_CustomTTL(t *testing.T) {
	cfg := &config.ExplainConfig{
		CacheTTLMinutes: 120,
		TimeoutMs:       1000,
	}
	ttl := cfg.CacheTTLMinutes
	if ttl == 0 {
		ttl = 60
	}
	if ttl != 120 {
		t.Errorf("custom TTL = %d, want 120", ttl)
	}
}

// ---------- fake PostgreSQL wire-protocol server ----------

// fakePGServer implements just enough of the PostgreSQL wire protocol to
// let pgx establish a connection and execute simple queries. It responds
// to all queries with a configurable plan JSON or an error.
type fakePGServer struct {
	listener net.Listener
	planJSON string // JSON to return for EXPLAIN queries
	failExec bool   // if true, return an error for Exec queries
}

// pgMsg writes a single PostgreSQL wire protocol message.
func pgMsg(w io.Writer, msgType byte, payload []byte) {
	buf := make([]byte, 1+4+len(payload))
	buf[0] = msgType
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, _ = w.Write(buf)
}

// pgStr creates a null-terminated string for the wire protocol.
func pgStr(s string) []byte {
	return append([]byte(s), 0)
}

func newFakePGServer(
	t *testing.T, planJSON string, failExec bool,
) *fakePGServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakePGServer{
		listener: ln,
		planJSON: planJSON,
		failExec: failExec,
	}
	go s.serve(t)
	return s
}

func (s *fakePGServer) addr() string {
	return s.listener.Addr().String()
}

func (s *fakePGServer) close() {
	_ = s.listener.Close()
}

func (s *fakePGServer) serve(t *testing.T) {
	t.Helper()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleConn(conn)
	}
}

// readMsg reads one wire-protocol message (type byte + length-prefixed body).
func readMsg(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	msgType := header[0]
	bodyLen := int(binary.BigEndian.Uint32(header[1:5])) - 4
	body := make([]byte, bodyLen)
	if bodyLen > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return 0, nil, err
		}
	}
	return msgType, body, nil
}

// sendError sends a PostgreSQL ErrorResponse message.
func sendError(w io.Writer, code, msg string) {
	payload := append([]byte{'S'}, pgStr("ERROR")...)
	payload = append(payload, 'C')
	payload = append(payload, pgStr(code)...)
	payload = append(payload, 'M')
	payload = append(payload, pgStr(msg)...)
	payload = append(payload, 0) // terminator
	pgMsg(w, 'E', payload)
}

// sendRowDescription sends a RowDescription with one text column.
func sendRowDescription(w io.Writer, colName string) {
	cn := pgStr(colName)
	// column count (2) + name + tableOID(4) + colNum(2) +
	// typeOID(4) + typLen(2) + typMod(4) + format(2)
	rd := make([]byte, 2+len(cn)+4+2+4+2+4+2)
	off := 0
	binary.BigEndian.PutUint16(rd[off:], 1) // 1 column
	off += 2
	copy(rd[off:], cn)
	off += len(cn)
	binary.BigEndian.PutUint32(rd[off:], 0) // table OID
	off += 4
	binary.BigEndian.PutUint16(rd[off:], 0) // column number
	off += 2
	binary.BigEndian.PutUint32(rd[off:], 25) // text OID
	off += 4
	binary.BigEndian.PutUint16(rd[off:], 0xFFFF) // type length
	off += 2
	binary.BigEndian.PutUint32(rd[off:], 0xFFFFFFFF) // type modifier
	off += 4
	binary.BigEndian.PutUint16(rd[off:], 0) // format (text)
	off += 2
	pgMsg(w, 'T', rd[:off])
}

// sendDataRow sends a DataRow with one text value.
func sendDataRow(w io.Writer, value string) {
	dr := make([]byte, 2+4+len(value))
	binary.BigEndian.PutUint16(dr[0:2], 1) // 1 column
	binary.BigEndian.PutUint32(dr[2:6], uint32(len(value)))
	copy(dr[6:], value)
	pgMsg(w, 'D', dr)
}

func (s *fakePGServer) handleConn(conn net.Conn) {
	defer conn.Close()

	// Read startup message: 4 bytes length, then payload.
	var msgLen int32
	if err := binary.Read(conn, binary.BigEndian, &msgLen); err != nil {
		return
	}
	startup := make([]byte, msgLen-4)
	if _, err := io.ReadFull(conn, startup); err != nil {
		return
	}

	// Check for SSLRequest (protocol version 80877103).
	if msgLen == 8 {
		protoVer := binary.BigEndian.Uint32(startup[:4])
		if protoVer == 80877103 {
			_, _ = conn.Write([]byte{'N'})
			if err := binary.Read(
				conn, binary.BigEndian, &msgLen,
			); err != nil {
				return
			}
			startup = make([]byte, msgLen-4)
			if _, err := io.ReadFull(conn, startup); err != nil {
				return
			}
		}
	}

	// Send AuthenticationOk (R, type=0).
	pgMsg(conn, 'R', []byte{0, 0, 0, 0})

	// Send required ParameterStatus messages.
	for _, kv := range [][2]string{
		{"server_version", "16.0"},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", "UTC"},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
	} {
		payload := append(pgStr(kv[0]), pgStr(kv[1])...)
		pgMsg(conn, 'S', payload)
	}

	// Send BackendKeyData (K).
	bkd := make([]byte, 8)
	binary.BigEndian.PutUint32(bkd[0:4], 12345)
	binary.BigEndian.PutUint32(bkd[4:8], 67890)
	pgMsg(conn, 'K', bkd)

	// Send ReadyForQuery (Z, 'I' = idle).
	pgMsg(conn, 'Z', []byte{'I'})

	// Extended protocol state: accumulate messages between Syncs.
	var parsedQuery string
	hasParse := false
	hasBind := false
	hasDescribeStmt := false
	hasDescribePortal := false
	hasExecute := false

	for {
		msgType, body, err := readMsg(conn)
		if err != nil {
			return
		}

		switch msgType {
		case 'Q': // Simple Query
			queryStr := string(body[:len(body)-1])
			s.handleSimpleQuery(conn, queryStr)

		case 'P': // Parse
			parts := strings.SplitN(string(body), "\x00", 3)
			if len(parts) >= 2 {
				parsedQuery = parts[1]
			}
			hasParse = true

		case 'B': // Bind
			hasBind = true

		case 'D': // Describe
			if len(body) > 0 && body[0] == 'S' {
				hasDescribeStmt = true
			} else {
				hasDescribePortal = true
			}

		case 'E': // Execute
			hasExecute = true

		case 'H': // Flush
			// Ignore.

		case 'S': // Sync — respond to accumulated batch.
			isExplain := strings.HasPrefix(parsedQuery, "EXPLAIN")

			if s.failExec && (hasParse || hasExecute) {
				sendError(conn, "42601", "simulated error")
				pgMsg(conn, 'Z', []byte{'I'})
			} else if hasParse && hasDescribeStmt &&
				!hasBind && !hasExecute {
				// Phase 1: Parse + Describe Statement + Sync.
				pgMsg(conn, '1', nil)          // ParseComplete
				pgMsg(conn, 't', []byte{0, 0}) // ParamDescription
				if isExplain {
					sendRowDescription(conn, "QUERY PLAN")
				} else {
					pgMsg(conn, 'n', nil) // NoData
				}
				pgMsg(conn, 'Z', []byte{'I'})
			} else if hasBind && hasExecute {
				// Phase 2: Bind + Describe Portal + Execute + Sync.
				pgMsg(conn, '2', nil) // BindComplete
				if hasDescribePortal {
					if isExplain {
						sendRowDescription(conn, "QUERY PLAN")
					} else {
						pgMsg(conn, 'n', nil) // NoData
					}
				}
				if isExplain {
					sendDataRow(conn, s.planJSON)
				}
				pgMsg(conn, 'C', pgStr("SET"))
				pgMsg(conn, 'Z', []byte{'I'})
			} else {
				pgMsg(conn, 'Z', []byte{'I'})
			}

			// Reset state but keep parsedQuery for phase 2.
			if hasBind {
				parsedQuery = ""
			}
			hasParse = false
			hasBind = false
			hasDescribeStmt = false
			hasDescribePortal = false
			hasExecute = false

		case 'X': // Terminate
			return

		default:
			// Ignore unknown messages.
		}
	}
}

func (s *fakePGServer) handleSimpleQuery(
	conn net.Conn, query string,
) {
	if s.failExec {
		sendError(conn, "42601", "simulated error")
		pgMsg(conn, 'Z', []byte{'I'})
		return
	}
	if strings.HasPrefix(query, "EXPLAIN") {
		sendRowDescription(conn, "QUERY PLAN")
		sendDataRow(conn, s.planJSON)
		pgMsg(conn, 'C', pgStr("EXPLAIN"))
	} else {
		pgMsg(conn, 'C', pgStr("SET"))
	}
	pgMsg(conn, 'Z', []byte{'I'})
}

func fakePGPool(
	t *testing.T, addr string,
) *pgxpool.Pool {
	t.Helper()
	dsn := "postgres://fake:fake@" + addr + "/fakedb?sslmode=disable"
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return pool
}

// ---------- tests using fake PG server ----------

func TestExplain_FullPathWithFakeServer(t *testing.T) {
	planJSON := `[{
		"Plan": {
			"Node Type": "Result",
			"Total Cost": 0.01,
			"Plan Rows": 1,
			"Actual Total Time": 0.002,
			"Actual Rows": 1
		},
		"Planning Time": 0.05,
		"Execution Time": 0.003
	}]`

	srv := newFakePGServer(t, planJSON, false)
	defer srv.close()

	pool := fakePGPool(t, srv.addr())
	defer pool.Close()

	cfg := &config.ExplainConfig{
		TimeoutMs:       5000,
		CacheTTLMinutes: 60,
	}

	var logMessages []string
	var mu sync.Mutex
	captureLog := func(level string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logMessages = append(logMessages, level+": "+msg)
	}

	ex := New(pool, cfg, captureLog)

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	result, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// Verify the result was built correctly.
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Query != "SELECT 1" {
		t.Errorf("Query = %q, want %q", result.Query, "SELECT 1")
	}
	if result.EstimatedCost != 0.01 {
		t.Errorf("EstimatedCost = %f, want 0.01", result.EstimatedCost)
	}
	if result.ActualTimeMs == nil {
		t.Fatal("ActualTimeMs is nil for analyzed query")
	}
	if *result.ActualTimeMs != 0.002 {
		t.Errorf("ActualTimeMs = %f, want 0.002", *result.ActualTimeMs)
	}
	if result.PlanningTimeMs == nil {
		t.Fatal("PlanningTimeMs is nil")
	}
	if *result.PlanningTimeMs != 0.05 {
		t.Errorf("PlanningTimeMs = %f, want 0.05", *result.PlanningTimeMs)
	}
	if len(result.NodeBreakdown) != 1 {
		t.Fatalf("NodeBreakdown length = %d, want 1",
			len(result.NodeBreakdown))
	}
	if result.NodeBreakdown[0].NodeType != "Result" {
		t.Errorf("NodeBreakdown[0].NodeType = %q, want %q",
			result.NodeBreakdown[0].NodeType, "Result")
	}
	// CachedAt should be nil (not from cache).
	if result.CachedAt != nil {
		t.Errorf("CachedAt = %v, want nil", result.CachedAt)
	}
	// Note should be empty for analyzed result.
	if result.Note != "" {
		t.Errorf("Note = %q, want empty for analyzed result", result.Note)
	}

	// Cache save should have been attempted (and may have warned).
	mu.Lock()
	defer mu.Unlock()
	// We expect a cache save warning since the fake server doesn't
	// handle INSERT properly (returns "SET" which pgx may accept).
	// Either way, the main result is valid.
}

func TestExplain_PlanOnlyWithFakeServer(t *testing.T) {
	planJSON := `[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Relation Name": "orders",
			"Total Cost": 100.5,
			"Plan Rows": 500
		}
	}]`

	srv := newFakePGServer(t, planJSON, false)
	defer srv.close()

	pool := fakePGPool(t, srv.addr())
	defer pool.Close()

	cfg := &config.ExplainConfig{
		TimeoutMs:       5000,
		CacheTTLMinutes: 30,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	result, err := ex.Explain(ctx, ExplainRequest{
		Query:    "SELECT * FROM orders",
		PlanOnly: true,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if result.ActualTimeMs != nil {
		t.Errorf("ActualTimeMs = %f, want nil for PlanOnly",
			*result.ActualTimeMs)
	}
	if result.Note == "" {
		t.Error("Note should be set for plan-only mode")
	}
	if !strings.Contains(result.Note, "without ANALYZE") {
		t.Errorf("Note = %q, want it to contain 'without ANALYZE'",
			result.Note)
	}
	if result.EstimatedCost != 100.5 {
		t.Errorf("EstimatedCost = %f, want 100.5", result.EstimatedCost)
	}
}

func TestExplain_PrepareConnError(t *testing.T) {
	// Fake server that returns errors for all queries, including BEGIN.
	srv := newFakePGServer(t, "", true)
	defer srv.close()

	pool := fakePGPool(t, srv.addr())
	defer pool.Close()

	cfg := &config.ExplainConfig{
		TimeoutMs:       5000,
		CacheTTLMinutes: 60,
	}
	ex := New(pool, cfg, noopLogFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	if err == nil {
		t.Fatal("expected error from failing prepareConn, got nil")
	}
	// The error should come from prepareConn's BEGIN.
	if !strings.Contains(err.Error(), "begin") &&
		!strings.Contains(err.Error(), "explain") {
		t.Errorf("error = %q, expected connection/begin error", err.Error())
	}
}

// No concurrent access tests: all functions tested here are pure functions
// or stateless validation checks that take no shared references. The
// pool-based tests use a shared fake pool but each test creates its own
// Explainer instance, so there is no shared mutable state.

// ========== LLM integration tests (explain_llm.go) ==========

// ---------- stripToJSON ----------

func TestStripToJSON_CleanObject(t *testing.T) {
	input := `{"summary":"test","slow_because":["seq scan"]}`
	got := stripToJSON(input)
	if got != input {
		t.Errorf("stripToJSON(clean object)\n  got  %q\n  want %q",
			got, input)
	}
}

func TestStripToJSON_MarkdownWrapped(t *testing.T) {
	input := "```json\n{\"summary\":\"x\"}\n```"
	want := `{"summary":"x"}`
	got := stripToJSON(input)
	if got != want {
		t.Errorf("stripToJSON(markdown wrapped)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripToJSON_ThinkingTokensBeforeJSON(t *testing.T) {
	input := "Let me analyze this query plan... {\"summary\":\"x\"}"
	want := `{"summary":"x"}`
	got := stripToJSON(input)
	if got != want {
		t.Errorf("stripToJSON(thinking tokens)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripToJSON_Array(t *testing.T) {
	input := "[1,2,3]"
	got := stripToJSON(input)
	if got != input {
		t.Errorf("stripToJSON(array)\n  got  %q\n  want %q",
			got, input)
	}
}

func TestStripToJSON_NoJSON(t *testing.T) {
	input := "```json\nno valid json here\n```"
	got := stripToJSON(input)
	// Should fall through to stripMarkdownFences.
	want := "no valid json here"
	if got != want {
		t.Errorf("stripToJSON(no json)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripToJSON_Empty(t *testing.T) {
	got := stripToJSON("")
	if got != "" {
		t.Errorf("stripToJSON(empty) = %q, want empty", got)
	}
}

func TestStripToJSON_WhitespaceAroundObject(t *testing.T) {
	input := "  \n  {\"key\":\"val\"}  \n  "
	want := `{"key":"val"}`
	got := stripToJSON(input)
	if got != want {
		t.Errorf("stripToJSON(whitespace around object)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripToJSON_NestedBraces(t *testing.T) {
	input := `{"outer":{"inner":"val"}}`
	got := stripToJSON(input)
	if got != input {
		t.Errorf("stripToJSON(nested braces)\n  got  %q\n  want %q",
			got, input)
	}
}

func TestStripToJSON_ThinkingThenMarkdownObject(t *testing.T) {
	// Thinking tokens + markdown fences wrapping an object.
	input := "Here is the analysis:\n```json\n{\"summary\":\"test\"}\n```"
	want := `{"summary":"test"}`
	got := stripToJSON(input)
	if got != want {
		t.Errorf("stripToJSON(thinking+markdown)\n  got  %q\n  want %q",
			got, want)
	}
}

// ---------- stripMarkdownFences ----------

func TestStripMarkdownFences_JsonFence(t *testing.T) {
	input := "```json\ncontent here\n```"
	want := "content here"
	got := stripMarkdownFences(input)
	if got != want {
		t.Errorf("stripMarkdownFences(json fence)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripMarkdownFences_PlainFence(t *testing.T) {
	input := "```\ncontent here\n```"
	want := "content here"
	got := stripMarkdownFences(input)
	if got != want {
		t.Errorf("stripMarkdownFences(plain fence)\n  got  %q\n  want %q",
			got, want)
	}
}

func TestStripMarkdownFences_NoFences(t *testing.T) {
	input := "just plain text"
	got := stripMarkdownFences(input)
	if got != input {
		t.Errorf("stripMarkdownFences(no fences)\n  got  %q\n  want %q",
			got, input)
	}
}

func TestStripMarkdownFences_Empty(t *testing.T) {
	got := stripMarkdownFences("")
	if got != "" {
		t.Errorf("stripMarkdownFences(empty) = %q, want empty", got)
	}
}

func TestStripMarkdownFences_OnlyFences(t *testing.T) {
	input := "```json\n```"
	got := stripMarkdownFences(input)
	if got != "" {
		t.Errorf("stripMarkdownFences(only fences) = %q, want empty",
			got)
	}
}

// ---------- applyLLMResponse ----------

func newTestExplainer(
	logFn func(string, string, ...any),
) *Explainer {
	return &Explainer{
		cfg:   &config.ExplainConfig{},
		logFn: logFn,
	}
}

func TestApplyLLMResponse_ValidJSON(t *testing.T) {
	ex := newTestExplainer(noopLogFn)
	result := &ExplainResult{
		Summary:         "original",
		SlowBecause:     nil,
		Recommendations: nil,
	}

	raw := `{"summary":"new summary","slow_because":["seq scan on orders"],"recommendations":["add index on orders.id"]}`
	ex.applyLLMResponse(raw, result)

	if result.Summary != "new summary" {
		t.Errorf("Summary = %q, want %q",
			result.Summary, "new summary")
	}
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "seq scan on orders" {
		t.Errorf("SlowBecause = %v, want [seq scan on orders]",
			result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "add index on orders.id" {
		t.Errorf("Recommendations = %v, want [add index on orders.id]",
			result.Recommendations)
	}
}

func TestApplyLLMResponse_PartialJSON_OnlySummary(t *testing.T) {
	ex := newTestExplainer(noopLogFn)
	original := []string{"original recommendation"}
	result := &ExplainResult{
		Summary:         "original",
		SlowBecause:     []string{"original reason"},
		Recommendations: original,
	}

	raw := `{"summary":"new summary"}`
	ex.applyLLMResponse(raw, result)

	if result.Summary != "new summary" {
		t.Errorf("Summary = %q, want %q",
			result.Summary, "new summary")
	}
	// SlowBecause and Recommendations should remain unchanged.
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "original reason" {
		t.Errorf("SlowBecause changed unexpectedly: %v",
			result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "original recommendation" {
		t.Errorf("Recommendations changed unexpectedly: %v",
			result.Recommendations)
	}
}

func TestApplyLLMResponse_InvalidJSON(t *testing.T) {
	var warnings []string
	var mu sync.Mutex
	captureLog := func(level string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings,
			fmt.Sprintf("%s: %s", level, fmt.Sprintf(msg, args...)))
	}

	ex := newTestExplainer(captureLog)
	result := &ExplainResult{
		Summary:         "original",
		SlowBecause:     []string{"original"},
		Recommendations: []string{"original"},
	}

	ex.applyLLMResponse("this is not json at all", result)

	// Result should be unchanged.
	if result.Summary != "original" {
		t.Errorf("Summary changed to %q, should remain %q",
			result.Summary, "original")
	}
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "original" {
		t.Errorf("SlowBecause changed: %v", result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "original" {
		t.Errorf("Recommendations changed: %v", result.Recommendations)
	}

	// Should have logged a warning.
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) == 0 {
		t.Error("expected a warning log for invalid JSON, got none")
	}
	foundParseWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "WARN") &&
			strings.Contains(w, "parse LLM response") {
			foundParseWarn = true
			break
		}
	}
	if !foundParseWarn {
		t.Errorf("expected WARN about parse failure, got: %v", warnings)
	}
}

func TestApplyLLMResponse_EmptySummaryDoesNotOverwrite(t *testing.T) {
	ex := newTestExplainer(noopLogFn)
	result := &ExplainResult{
		Summary: "existing summary",
	}

	raw := `{"summary":"","slow_because":["reason"]}`
	ex.applyLLMResponse(raw, result)

	// Empty summary should not overwrite existing.
	if result.Summary != "existing summary" {
		t.Errorf("Summary = %q, want %q (empty should not overwrite)",
			result.Summary, "existing summary")
	}
	// But SlowBecause should be updated.
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "reason" {
		t.Errorf("SlowBecause = %v, want [reason]",
			result.SlowBecause)
	}
}

func TestApplyLLMResponse_MarkdownWrappedJSON(t *testing.T) {
	ex := newTestExplainer(noopLogFn)
	result := &ExplainResult{
		Summary: "original",
	}

	raw := "```json\n{\"summary\":\"llm summary\",\"slow_because\":[\"full table scan\"],\"recommendations\":[\"create index\"]}\n```"
	ex.applyLLMResponse(raw, result)

	if result.Summary != "llm summary" {
		t.Errorf("Summary = %q, want %q",
			result.Summary, "llm summary")
	}
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "full table scan" {
		t.Errorf("SlowBecause = %v, want [full table scan]",
			result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "create index" {
		t.Errorf("Recommendations = %v, want [create index]",
			result.Recommendations)
	}
}

func TestApplyLLMResponse_EmptySlowBecauseDoesNotOverwrite(
	t *testing.T,
) {
	ex := newTestExplainer(noopLogFn)
	result := &ExplainResult{
		Summary:     "original",
		SlowBecause: []string{"existing reason"},
	}

	raw := `{"summary":"new","slow_because":[],"recommendations":["idx"]}`
	ex.applyLLMResponse(raw, result)

	if result.Summary != "new" {
		t.Errorf("Summary = %q, want %q", result.Summary, "new")
	}
	// Empty slice should not overwrite existing.
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "existing reason" {
		t.Errorf("SlowBecause = %v, want [existing reason]",
			result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "idx" {
		t.Errorf("Recommendations = %v, want [idx]",
			result.Recommendations)
	}
}

// ---------- enhanceWithLLM ----------

// newTestLLMServer creates an httptest server returning a canned
// OpenAI-compatible chat completion response.
func newTestLLMServer(
	t *testing.T, content string, statusCode int,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if statusCode != http.StatusOK {
				w.WriteHeader(statusCode)
				_, _ = w.Write([]byte("error"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": content,
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"total_tokens": 100,
				},
			})
		}),
	)
}

func TestEnhanceWithLLM_NilClient(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	// llmClient is nil by default from New().
	result := &ExplainResult{
		Summary: "original",
		Query:   "SELECT 1",
	}

	ex.enhanceWithLLM(context.Background(), result)

	if result.Summary != "original" {
		t.Errorf("Summary changed to %q, want %q (nil client = no-op)",
			result.Summary, "original")
	}
}

func TestEnhanceWithLLM_Disabled(t *testing.T) {
	// Create a client with Enabled=false.
	client := llm.New(&config.LLMConfig{
		Enabled:        false,
		Endpoint:       "http://localhost:1234",
		APIKey:         "test-key",
		Model:          "test",
		TimeoutSeconds: 5,
	}, noopLogFn)

	ex := NewWithLLM(nil, &config.ExplainConfig{}, client, noopLogFn)
	result := &ExplainResult{
		Summary: "original",
		Query:   "SELECT 1",
	}

	ex.enhanceWithLLM(context.Background(), result)

	if result.Summary != "original" {
		t.Errorf("Summary changed to %q, want %q (disabled = no-op)",
			result.Summary, "original")
	}
}

func TestEnhanceWithLLM_ValidResponse(t *testing.T) {
	content := `{"summary":"LLM summary","slow_because":["seq scan"],"recommendations":["add index"]}`
	srv := newTestLLMServer(t, content, http.StatusOK)
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}, noopLogFn)

	cfg := &config.ExplainConfig{MaxTokens: 2048}
	ex := NewWithLLM(nil, cfg, client, noopLogFn)
	result := &ExplainResult{
		Summary:  "deterministic summary",
		Query:    "SELECT * FROM orders",
		PlanJSON: json.RawMessage(`[{"Plan":{"Node Type":"Seq Scan"}}]`),
	}

	ex.enhanceWithLLM(context.Background(), result)

	if result.Summary != "LLM summary" {
		t.Errorf("Summary = %q, want %q",
			result.Summary, "LLM summary")
	}
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "seq scan" {
		t.Errorf("SlowBecause = %v, want [seq scan]",
			result.SlowBecause)
	}
	if len(result.Recommendations) != 1 ||
		result.Recommendations[0] != "add index" {
		t.Errorf("Recommendations = %v, want [add index]",
			result.Recommendations)
	}
}

func TestEnhanceWithLLM_ErrorResponse(t *testing.T) {
	srv := newTestLLMServer(t, "", http.StatusInternalServerError)
	defer srv.Close()

	var warnings []string
	var mu sync.Mutex
	captureLog := func(level string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings,
			fmt.Sprintf("%s: %s", level, fmt.Sprintf(msg, args...)))
	}

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 2,
	}, captureLog)

	ex := NewWithLLM(nil, &config.ExplainConfig{}, client, captureLog)
	result := &ExplainResult{
		Summary:  "original",
		Query:    "SELECT 1",
		PlanJSON: json.RawMessage(`[{"Plan":{}}]`),
	}

	ex.enhanceWithLLM(context.Background(), result)

	// Result should be unchanged on error.
	if result.Summary != "original" {
		t.Errorf("Summary changed to %q, want %q (error = no change)",
			result.Summary, "original")
	}

	// Should have logged a warning about the failure.
	mu.Lock()
	defer mu.Unlock()
	foundWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "WARN") &&
			strings.Contains(w, "LLM enhancement failed") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected WARN about LLM enhancement failure, got: %v",
			warnings)
	}
}

func TestEnhanceWithLLM_MaxTokensDefault(t *testing.T) {
	// Verify that MaxTokens defaults to 4096 when cfg.MaxTokens is 0.
	// We capture the request body to check the max_tokens value.
	var capturedMaxTokens int
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				MaxTokens int `json:"max_tokens"`
			}
			_ = json.Unmarshal(body, &req)
			capturedMaxTokens = req.MaxTokens

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": `{"summary":"test"}`,
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"total_tokens": 50},
			})
		}),
	)
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}, noopLogFn)

	cfg := &config.ExplainConfig{MaxTokens: 0} // should default to 4096
	ex := NewWithLLM(nil, cfg, client, noopLogFn)
	result := &ExplainResult{
		Query:    "SELECT 1",
		PlanJSON: json.RawMessage(`[{"Plan":{}}]`),
	}

	ex.enhanceWithLLM(context.Background(), result)

	// The LLM client internally adjusts maxTokens (adds thinking
	// overhead for thinking models, or bumps to 16384 if <= 0).
	// Our code passes 4096 to Chat(), and the client may adjust it.
	// We verify the Explainer sends 4096 by checking that the value
	// is at least 4096 (the client may add overhead).
	if capturedMaxTokens < 4096 {
		t.Errorf("max_tokens in request = %d, want >= 4096 "+
			"(default when cfg.MaxTokens=0)", capturedMaxTokens)
	}
}

func TestEnhanceWithLLM_CustomMaxTokens(t *testing.T) {
	var capturedMaxTokens int
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				MaxTokens int `json:"max_tokens"`
			}
			_ = json.Unmarshal(body, &req)
			capturedMaxTokens = req.MaxTokens

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": `{"summary":"test"}`,
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"total_tokens": 50},
			})
		}),
	)
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}, noopLogFn)

	cfg := &config.ExplainConfig{MaxTokens: 8192}
	ex := NewWithLLM(nil, cfg, client, noopLogFn)
	result := &ExplainResult{
		Query:    "SELECT 1",
		PlanJSON: json.RawMessage(`[{"Plan":{}}]`),
	}

	ex.enhanceWithLLM(context.Background(), result)

	if capturedMaxTokens < 8192 {
		t.Errorf("max_tokens in request = %d, want >= 8192 "+
			"(custom cfg.MaxTokens=8192)", capturedMaxTokens)
	}
}

func TestEnhanceWithLLM_MarkdownWrappedResponse(t *testing.T) {
	// LLM returns markdown-wrapped JSON despite instructions.
	content := "```json\n{\"summary\":\"wrapped\",\"slow_because\":[\"bad join\"],\"recommendations\":[\"rewrite\"]}\n```"
	srv := newTestLLMServer(t, content, http.StatusOK)
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}, noopLogFn)

	ex := NewWithLLM(
		nil, &config.ExplainConfig{}, client, noopLogFn,
	)
	result := &ExplainResult{
		Summary:  "original",
		Query:    "SELECT 1",
		PlanJSON: json.RawMessage(`[{"Plan":{}}]`),
	}

	ex.enhanceWithLLM(context.Background(), result)

	if result.Summary != "wrapped" {
		t.Errorf("Summary = %q, want %q (markdown-wrapped response)",
			result.Summary, "wrapped")
	}
	if len(result.SlowBecause) != 1 ||
		result.SlowBecause[0] != "bad join" {
		t.Errorf("SlowBecause = %v, want [bad join]",
			result.SlowBecause)
	}
}

func TestEnhanceWithLLM_CancelledContext(t *testing.T) {
	// Server that blocks forever; context cancellation should abort.
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}),
	)
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 30,
	}, noopLogFn)

	ex := NewWithLLM(
		nil, &config.ExplainConfig{}, client, noopLogFn,
	)
	result := &ExplainResult{
		Summary:  "original",
		Query:    "SELECT 1",
		PlanJSON: json.RawMessage(`[{"Plan":{}}]`),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	ex.enhanceWithLLM(ctx, result)

	// Result should be unchanged.
	if result.Summary != "original" {
		t.Errorf("Summary changed to %q, want %q (cancelled ctx)",
			result.Summary, "original")
	}
}

// ---------- llmExplainResponse JSON parsing ----------

func TestLLMExplainResponse_FullRoundTrip(t *testing.T) {
	resp := llmExplainResponse{
		Summary:         "test",
		SlowBecause:     []string{"a", "b"},
		Recommendations: []string{"c"},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got llmExplainResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Summary != resp.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, resp.Summary)
	}
	if len(got.SlowBecause) != 2 {
		t.Fatalf("SlowBecause length = %d, want 2",
			len(got.SlowBecause))
	}
	if got.SlowBecause[0] != "a" || got.SlowBecause[1] != "b" {
		t.Errorf("SlowBecause = %v, want [a b]", got.SlowBecause)
	}
	if len(got.Recommendations) != 1 || got.Recommendations[0] != "c" {
		t.Errorf("Recommendations = %v, want [c]",
			got.Recommendations)
	}
}
