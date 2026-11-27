package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/planhash"
)

// collection-09: both plans of a pair are hashed with one resolver, so a
// daily partition swap is not a plan change, and a pair whose plans
// resolved differently (v2 versus v2u) is not compared at all.

// dayRoots maps the daily partitions of events to their root; a name in
// missing is unresolved (for example a partition dropped since).
type dayRoots struct{ missing string }

func (d dayRoots) Root(schema, rel string) (planhash.Root, error) {
	if rel == d.missing {
		return planhash.Root{}, planhash.ErrUnresolved
	}
	if strings.HasPrefix(rel, "events_p") {
		return planhash.Root{Schema: schema, Name: "events", Leaves: 30}, nil
	}
	return planhash.Root{Schema: schema, Name: rel}, nil
}

func dayPlan(day string, cost string) []byte {
	return []byte(`[{"Plan": {"Node Type": "Seq Scan", "Schema": "public",
	  "Relation Name": "events_p` + day + `", "Total Cost": ` + cost + `}}]`)
}

func TestPlanRegression_PartitionSwapIsNotAPlanChange(t *testing.T) {
	f := planHashFinding(t, planPair{
		QueryID: 21, QueryText: "select",
		CurrentPlan: dayPlan("20261106", "5000.0"), CurrentCost: 5000,
		PreviousPlan: dayPlan("20261105", "100.0"), PreviousCost: 100,
		Roots: dayRoots{},
	})
	cur, _ := f.Detail["current_plan_hash"].(string)
	if !strings.HasPrefix(cur, "v2:") || cur != f.Detail["previous_plan_hash"] {
		t.Fatalf("hashes current=%v previous=%v, want one resolved v2 hash",
			f.Detail["current_plan_hash"], f.Detail["previous_plan_hash"])
	}
	if changed, ok := f.Detail["plan_changed"].(bool); !ok || changed {
		t.Fatalf("plan_changed = %v, want false for a partition swap", f.Detail["plan_changed"])
	}
}

func TestPlanRegression_MixedResolutionIsNotCompared(t *testing.T) {
	f := planHashFinding(t, planPair{
		QueryID: 22, QueryText: "select",
		CurrentPlan: dayPlan("20261106", "5000.0"), CurrentCost: 5000,
		PreviousPlan: dayPlan("20261105", "100.0"), PreviousCost: 100,
		Roots: dayRoots{missing: "events_p20261105"},
	})
	cur, _ := f.Detail["current_plan_hash"].(string)
	prev, _ := f.Detail["previous_plan_hash"].(string)
	if !strings.HasPrefix(cur, "v2:") || !strings.HasPrefix(prev, "v2u:") {
		t.Fatalf("hashes current=%q previous=%q, want v2 and v2u", cur, prev)
	}
	if v, ok := f.Detail["plan_changed"]; ok {
		t.Fatalf("plan_changed = %v, want absent for incomparable fingerprints", v)
	}
}

func TestHashVersion(t *testing.T) {
	for in, want := range map[string]string{"v2:ab": "v2", "v2u:ab": "v2u", "": "",
		"nocolon": "nocolon"} {
		if got := hashVersion(in); got != want {
			t.Errorf("hashVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
