package planhash

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// evid-02b: Shape keeps a plan's structure and its row and cost numbers
// and drops every condition, filter, output list and alias. No concurrent
// access tests: Shape is a pure function of its input.

// goldenFingerprints were computed by Compute before Shape existed; Shape
// must never move them.
var goldenFingerprints = []struct{ plan, want string }{
	{basePlan, "v2u:dbbc995257b35b82da650e02b5283df7"},
	{`{"Node Type": "Seq Scan", "Relation Name": "t", "Total Cost": 1}`,
		"v2u:7509eb5554da62265a131d252cf5a9d3"},
	{`{"Plan": {"Node Type": "Append", "Plans": [{"Node Type": "Seq Scan", ` +
		`"Relation Name": "a"}, {"Node Type": "Seq Scan", "Relation Name": "b"}]}}`,
		"v2u:1eff5ae58a73e21774eeae29cac5609f"},
}

func TestCompute_GoldenFingerprintsUnchanged(t *testing.T) {
	for i, g := range goldenFingerprints {
		if got := mustCompute(t, g.plan); got != g.want {
			t.Errorf("golden %d: %s, want %s", i, got, g.want)
		}
	}
}

func mustShape(t *testing.T, plan string) map[string]any {
	t.Helper()
	out, err := Shape([]byte(plan))
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("Shape output %s is not JSON: %v", out, err)
	}
	return v
}

func TestShape_KeepsStructureAndNumbersDropsConditions(t *testing.T) {
	out, err := Shape([]byte(basePlan))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"customer_id", "open", "Filter", "Cond", "Alias",
		"Plan Width", "Startup Cost", "Rows Removed", "Execution Time", "\"o\"", "42)"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("shape holds %q: %s", leak, out)
		}
	}
	root := mustShape(t, basePlan)
	if root["Node Type"] != "Hash Join" || root["Join Type"] != "Inner" ||
		root["Parallel Aware"] != false || root["Total Cost"] != 120.25 ||
		root["Plan Rows"] != 42.0 || root["Actual Rows"] != 40.0 || root["Actual Loops"] != 1.0 {
		t.Fatalf("root %v", root)
	}
	kids, ok := root["Plans"].([]any)
	if !ok || len(kids) != 2 {
		t.Fatalf("children %v", root["Plans"])
	}
	seq := kids[0].(map[string]any)
	if seq["Relation Name"] != "orders" || seq["Schema"] != "public" ||
		seq["Parent Relationship"] != "Outer" || seq["Plan Rows"] != 1000.0 {
		t.Fatalf("seq scan %v", seq)
	}
	if _, has := seq["Actual Rows"]; has {
		t.Fatalf("seq scan has Actual Rows it never had: %v", seq)
	}
	idx := kids[1].(map[string]any)["Plans"].([]any)[0].(map[string]any)
	if idx["Index Name"] != "customers_pkey" || idx["Scan Direction"] != "Forward" {
		t.Fatalf("index scan %v", idx)
	}
}

func TestShape_IsDeterministicAndAcceptsEveryEnvelope(t *testing.T) {
	node := `{"Node Type": "Seq Scan", "Relation Name": "t", "Total Cost": 1.5}`
	want, err := Shape([]byte(node))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{node, `{"Plan": ` + node + `}`, `[{"Plan": ` + node + `}]`} {
		got, err := Shape([]byte(env))
		if err != nil || string(got) != string(want) {
			t.Fatalf("Shape(%s) = %s, %v; want %s", env, got, err, want)
		}
	}
	if string(want) != `{"Node Type":"Seq Scan","Relation Name":"t","Total Cost":1.5}` {
		t.Fatalf("shape %s, want sorted keys and the exact number", want)
	}
}

func TestShape_NumbersThatAreNotNumbersAreDropped(t *testing.T) {
	root := mustShape(t, `{"Node Type": "Result", "Plan Rows": "many", "Total Cost": null}`)
	if len(root) != 1 || root["Node Type"] != "Result" {
		t.Fatalf("shape %v, want only the node type", root)
	}
}

func TestShape_InvalidInput(t *testing.T) {
	deep := strings.Repeat(`{"Node Type": "Result", "Plans": [`, MaxDepth+1) +
		`{"Node Type": "Result"}` + strings.Repeat(`]}`, MaxDepth+1)
	for name, in := range map[string]string{
		"empty": "", "spaces": "   ", "not json": "Seq Scan on t", "number": "42",
		"empty array": "[]", "no node type": `{"Relation Name": "t"}`,
		"plans not array": `{"Node Type": "Result", "Plans": {}}`,
		"child not object": `{"Node Type": "Result", "Plans": [1]}`, "too deep": deep,
		"text explain": `[{"QUERY PLAN": "Seq Scan on t"}]`,
	} {
		out, err := Shape([]byte(in))
		if !errors.Is(err, ErrInvalidPlan) || out != nil {
			t.Errorf("%s: Shape = %s, %v; want ErrInvalidPlan", name, out, err)
		}
	}
}

func TestShape_DepthBoundaryIsAccepted(t *testing.T) {
	plan := strings.Repeat(`{"Node Type": "Result", "Plans": [`, MaxDepth-1) +
		`{"Node Type": "Result"}` + strings.Repeat(`]}`, MaxDepth-1)
	if _, err := Shape([]byte(plan)); err != nil {
		t.Fatalf("a plan of depth %d: %v", MaxDepth, err)
	}
}
