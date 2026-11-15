package planhash

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// UsesIndex and FindIndex (lab-01b) are the one walker of "Index Name"
// nodes: the optimizer's what-if guard and the verifier's mechanism gate
// both call them. Pure functions over bytes: no shared state, so no
// concurrent-access test applies.

const usesNestedPlan = `[{"Plan":{"Node Type":"Nested Loop","Plans":[
	{"Node Type":"Seq Scan"},
	{"Node Type":"Bitmap Heap Scan","Plans":[
		{"Node Type":"Bitmap Index Scan","Index Name":"orders_status_idx"}]}]}}]`

// The optimizer's planUsesIndex cases, unchanged: nested bitmap scan found,
// a prefix and an empty name never match, malformed JSON is no use.
func TestUsesIndexWalksNestedPlans(t *testing.T) {
	plan := []byte(usesNestedPlan)
	if !UsesIndex(plan, "orders_status_idx") {
		t.Fatal("nested bitmap index scan not found")
	}
	if UsesIndex(plan, "orders_status") || UsesIndex(plan, "") {
		t.Fatal("a prefix or an empty name matched")
	}
	if UsesIndex([]byte(`{"Node Type":"Index Scan","Index Name":""}`), "") {
		t.Fatal("an empty name matched a node with an empty Index Name")
	}
	if UsesIndex([]byte("not json"), "orders_status_idx") {
		t.Fatal("malformed plan reported a use")
	}
}

// The auto_explain object form and a bare node are plans too, as
// planRoots accepted them in internal/verify.
func TestFindIndexAcceptsEveryPlanForm(t *testing.T) {
	node := `{"Node Type":"Index Scan","Index Name":"t_a_idx","Relation Name":"t"}`
	forms := map[string]string{
		"array":  `[{"Plan":` + node + `}]`,
		"object": `{"Query Text":"SELECT 1","Plan":` + node + `}`,
		"bare":   node,
	}
	for name, doc := range forms {
		found, err := FindIndex([]byte(doc), "t_a_idx")
		if err != nil || !found {
			t.Errorf("%s form: found=%v err=%v, want found", name, found, err)
		}
		found, err = FindIndex([]byte(doc), "t_b_idx")
		if err != nil || found {
			t.Errorf("%s form, other index: found=%v err=%v, want not found", name, found,
				err)
		}
	}
}

// Invalid input: JSON that is not a plan is ErrNotPlan, never "not used".
func TestFindIndexRejectsWhatIsNotAPlan(t *testing.T) {
	for _, doc := range []string{`{"Query Text":"x"}`, `42`, `"Index Name"`, `[]`,
		`[{"Plan":7}]`, `null`, `not json`, ``} {
		found, err := FindIndex([]byte(doc), "t_a_idx")
		if found || !errors.Is(err, ErrNotPlan) {
			t.Errorf("FindIndex(%q) = %v, %v; want false, ErrNotPlan", doc, found, err)
		}
		if UsesIndex([]byte(doc), "t_a_idx") {
			t.Errorf("UsesIndex(%q) = true", doc)
		}
	}
}

// The walk reads node keys, not text: an index named only in a filter or
// an index condition is not a use.
func TestFindIndexIgnoresNamesInConditionText(t *testing.T) {
	doc := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t",
		"Filter":"(t_a_idx = 1)","Plans":[{"Node Type":"Index Scan",
		"Index Name":"other_idx","Index Cond":"(t_a_idx > 2)"}]}}]`
	found, err := FindIndex([]byte(doc), "t_a_idx")
	if err != nil || found {
		t.Fatalf("FindIndex = %v, %v; a name in condition text matched", found, err)
	}
	found, err = FindIndex([]byte(doc), "other_idx")
	if err != nil || !found {
		t.Fatalf("FindIndex(other_idx) = %v, %v; want the child's Index Name", found, err)
	}
}

// Subplans (InitPlan, SubPlan) are children under "Plans" like any other.
func TestFindIndexFindsSubplans(t *testing.T) {
	doc := `[{"Plan":{"Node Type":"Result","Plans":[{"Node Type":"Limit",
		"Parent Relationship":"InitPlan","Plans":[{"Node Type":"Index Only Scan",
		"Index Name":"t_pkey"}]}]}}]`
	if found, err := FindIndex([]byte(doc), "t_pkey"); err != nil || !found {
		t.Fatalf("index only scan in an InitPlan: %v, %v", found, err)
	}
}

// Boundary: nesting past MaxDepth is not walked forever; the error says so.
func TestFindIndexBoundsDepth(t *testing.T) {
	doc := `{"Node Type":"Index Scan","Index Name":"deep_idx"}`
	for i := 0; i < MaxDepth+5; i++ {
		doc = `{"Node Type":"Limit","Plans":[` + doc + `]}`
	}
	found, err := FindIndex([]byte(doc), "deep_idx")
	if found || err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("FindIndex past MaxDepth = %v, %v; want a depth error", found, err)
	}
}

func TestPlanRootsFindsEachForm(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`[{"Plan":{"Node Type":"A"}},{"Plan":{"Node Type":"B"}}]`),
		&doc); err != nil {
		t.Fatal(err)
	}
	roots := PlanRoots(doc)
	if len(roots) != 2 || roots[0]["Node Type"] != "A" || roots[1]["Node Type"] != "B" {
		t.Fatalf("PlanRoots = %v, want nodes A and B", roots)
	}
	if got := PlanRoots(map[string]any{"Plan": "x"}); len(got) != 0 {
		t.Fatalf("a Plan that is not a node gave roots %v", got)
	}
	if got := PlanRoots(nil); got != nil {
		t.Fatalf("PlanRoots(nil) = %v", got)
	}
}
