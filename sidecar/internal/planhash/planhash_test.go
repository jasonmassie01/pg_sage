package planhash

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// No concurrent access tests: Compute is a pure function of its input and
// holds no shared state.

const basePlan = `[{"Plan": {
  "Node Type": "Hash Join", "Parallel Aware": false, "Join Type": "Inner",
  "Startup Cost": 10.5, "Total Cost": 120.25, "Plan Rows": 42,
  "Plan Width": 16, "Actual Rows": 40, "Actual Loops": 1,
  "Hash Cond": "(o.customer_id = c.id)",
  "Plans": [
    {"Node Type": "Seq Scan", "Parent Relationship": "Outer",
     "Relation Name": "orders", "Schema": "public", "Alias": "o",
     "Total Cost": 80.0, "Plan Rows": 1000,
     "Filter": "(status = 'open'::text)", "Rows Removed by Filter": 12},
    {"Node Type": "Hash", "Parent Relationship": "Inner",
     "Total Cost": 20.0, "Plans": [
       {"Node Type": "Index Scan", "Parent Relationship": "Outer",
        "Scan Direction": "Forward", "Index Name": "customers_pkey",
        "Relation Name": "customers", "Schema": "public", "Alias": "c",
        "Index Cond": "(id = 42)", "Total Cost": 8.0}
     ]}
  ]},
  "Planning Time": 0.3, "Execution Time": 12.9}]`

var hashPattern = regexp.MustCompile(`^v1:[0-9a-f]{32}$`)

func mustCompute(t *testing.T, plan string) string {
	t.Helper()
	h, err := Compute([]byte(plan))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return h
}

func TestCompute_HappyPathFormatAndDeterminism(t *testing.T) {
	first := mustCompute(t, basePlan)
	if !hashPattern.MatchString(first) {
		t.Fatalf("hash %q does not match %s", first, hashPattern)
	}
	for i := 0; i < 5; i++ {
		if again := mustCompute(t, basePlan); again != first {
			t.Fatalf("run %d: hash %q != %q", i, again, first)
		}
	}
}

// Costs, row estimates, timings, literals in conditions, aliases and
// buffer counters change between executions of the same plan shape; the
// fingerprint must not.
func TestCompute_IgnoresVolatileFields(t *testing.T) {
	want := mustCompute(t, basePlan)
	volatile := strings.NewReplacer(
		`"Total Cost": 120.25`, `"Total Cost": 99999.5`,
		`"Plan Rows": 42`, `"Plan Rows": 7`,
		`"Actual Rows": 40`, `"Actual Rows": 4000`,
		`"Execution Time": 12.9`, `"Execution Time": 900.1`,
		`(id = 42)`, `(id = 77)`,
		`'open'::text`, `'closed'::text`,
		`"Alias": "o"`, `"Alias": "ord"`,
		`"Rows Removed by Filter": 12`, `"Shared Hit Blocks": 5`,
	).Replace(basePlan)
	if got := mustCompute(t, volatile); got != want {
		t.Errorf("volatile-only change altered hash: %q != %q", got, want)
	}
	spaced := strings.ReplaceAll(basePlan, ": ", ":   ")
	if got := mustCompute(t, spaced); got != want {
		t.Errorf("whitespace change altered hash")
	}
}

func TestCompute_DetectsShapeChanges(t *testing.T) {
	base := mustCompute(t, basePlan)
	cases := map[string][2]string{
		"node type":   {`"Node Type": "Seq Scan"`, `"Node Type": "Bitmap Heap Scan"`},
		"join type":   {`"Join Type": "Inner"`, `"Join Type": "Left"`},
		"index":       {`"customers_pkey"`, `"customers_email_idx"`},
		"relation":    {`"Relation Name": "orders"`, `"Relation Name": "orders_2026"`},
		"direction":   {`"Scan Direction": "Forward"`, `"Scan Direction": "Backward"`},
		"parallelism": {`"Parallel Aware": false`, `"Parallel Aware": true`},
		"parent role": {`"Parent Relationship": "Inner"`, `"Parent Relationship": "Outer"`},
	}
	for name, c := range cases {
		changed := strings.Replace(basePlan, c[0], c[1], 1)
		if changed == basePlan {
			t.Fatalf("%s: replacement %q not found in fixture", name, c[0])
		}
		if got := mustCompute(t, changed); got == base {
			t.Errorf("%s change kept the same hash %q", name, got)
		}
	}
}

func TestCompute_ChildOrderMatters(t *testing.T) {
	a := `{"Plan": {"Node Type": "Nested Loop", "Plans": [
	  {"Node Type": "Seq Scan", "Relation Name": "a"},
	  {"Node Type": "Seq Scan", "Relation Name": "b"}]}}`
	b := `{"Plan": {"Node Type": "Nested Loop", "Plans": [
	  {"Node Type": "Seq Scan", "Relation Name": "b"},
	  {"Node Type": "Seq Scan", "Relation Name": "a"}]}}`
	if mustCompute(t, a) == mustCompute(t, b) {
		t.Error("swapping outer and inner relations must change the hash")
	}
}

// EXPLAIN (FORMAT JSON) returns an array, auto_explain logs an object
// with "Plan", and callers may hold the bare root node.
func TestCompute_AcceptsAllPlanEnvelopes(t *testing.T) {
	node := `{"Node Type": "Seq Scan", "Relation Name": "t", "Total Cost": 1}`
	forms := []string{
		`[{"Plan": ` + node + `}]`,
		`{"Plan": ` + node + `, "Query Text": "select 1"}`,
		node,
	}
	want := mustCompute(t, forms[0])
	for _, f := range forms[1:] {
		if got := mustCompute(t, f); got != want {
			t.Errorf("envelope %s: %q != %q", f[:12], got, want)
		}
	}
}

func TestCompute_InvalidInput(t *testing.T) {
	cases := map[string]string{
		"nil-like empty":   "",
		"whitespace":       "   \n",
		"malformed json":   `[{"Plan": {"Node Type": "Seq Scan"`,
		"not a plan":       `{"foo": 1}`,
		"empty array":      `[]`,
		"array of scalars": `[1, 2]`,
		"node type number": `{"Node Type": 5}`,
		"empty node type":  `{"Node Type": ""}`,
		"child not object": `{"Node Type": "Append", "Plans": [1]}`,
		"plans not array":  `{"Node Type": "Append", "Plans": {"a": 1}}`,
		"scalar":           `42`,
		"null":             `null`,
	}
	for name, in := range cases {
		h, err := Compute([]byte(in))
		if err == nil {
			t.Errorf("%s: want error, got hash %q", name, h)
			continue
		}
		if !errors.Is(err, ErrInvalidPlan) {
			t.Errorf("%s: error %v is not ErrInvalidPlan", name, err)
		}
		if h != "" {
			t.Errorf("%s: hash %q returned with error", name, h)
		}
	}
	if _, err := Compute(nil); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("nil input: want ErrInvalidPlan, got %v", err)
	}
}

func nestedPlan(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"Node Type": "Subquery Scan", "Plans": [`)
	}
	b.WriteString(`{"Node Type": "Result"}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`]}`)
	}
	return b.String()
}

// Depth is bounded so a hostile or corrupt plan cannot exhaust the stack.
// MaxDepth counts plan nodes from the root (the root is depth 1).
func TestCompute_DepthBoundary(t *testing.T) {
	if _, err := Compute([]byte(nestedPlan(MaxDepth - 1))); err != nil {
		t.Fatalf("depth %d must be accepted: %v", MaxDepth, err)
	}
	_, err := Compute([]byte(nestedPlan(MaxDepth)))
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("depth %d: want ErrInvalidPlan, got %v", MaxDepth+1, err)
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error should name the depth limit: %v", err)
	}
}

// Structural strings are length-prefixed, so values containing the
// encoder's own delimiters cannot collide with a different tree.
func TestCompute_DelimiterInjectionDoesNotCollide(t *testing.T) {
	a := `{"Node Type": "Seq Scan", "Relation Name": "a\",b"}`
	b := `{"Node Type": "Seq Scan", "Relation Name": "a", "Schema": "b"}`
	if mustCompute(t, a) == mustCompute(t, b) {
		t.Error("distinct plans collided through delimiter characters")
	}
}

func ExampleCompute() {
	h, _ := Compute([]byte(`{"Node Type": "Seq Scan", "Relation Name": "t"}`))
	fmt.Println(strings.HasPrefix(h, "v1:"))
	// Output: true
}
