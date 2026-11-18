package policy

import "testing"

// plan-02b: loosening a plan cap or dropping a peak window grants plans
// more, so a proposal that does it widens (an agent's needs two people).
// No concurrent access test: Widens is a pure function.

func TestWidensPlanCaps(t *testing.T) {
	cases := []struct {
		name   string
		base   PlanCaps
		next   PlanCaps
		widens bool
	}{
		{"defaults both ways", PlanCaps{}, PlanCaps{}, false},
		{"explicit defaults", PlanCaps{}, DefaultPlanCaps(), false},
		{"raise indexes per table", PlanCaps{}, PlanCaps{MaxNewIndexesPerTable: 3}, true},
		{"lower indexes per table", PlanCaps{MaxNewIndexesPerTable: 3},
			PlanCaps{MaxNewIndexesPerTable: 1}, false},
		{"back to the default from a lower cap", PlanCaps{MaxNewIndexesPerTable: 1},
			PlanCaps{}, true},
		{"raise index bytes", PlanCaps{}, PlanCaps{MaxNewIndexBytesPct: 11}, true},
		{"lower index bytes", PlanCaps{}, PlanCaps{MaxNewIndexBytesPct: 9}, false},
		{"drop a peak window", PlanCaps{PeakWindows: []string{"always"}}, PlanCaps{}, true},
		{"add a peak window", PlanCaps{}, PlanCaps{PeakWindows: []string{"always"}}, false},
		{"swap a peak window", PlanCaps{PeakWindows: []string{"weekdays 09:00-17:00 UTC"}},
			PlanCaps{PeakWindows: []string{"weekends 09:00-17:00 UTC"}}, true},
	}
	for _, c := range cases {
		base, next := StaffedProfile(), StaffedProfile()
		base.Plans, next.Plans = c.base, c.next
		if got := Widens(base, next); got != c.widens {
			t.Errorf("%s: Widens = %v, want %v", c.name, got, c.widens)
		}
	}
}

// The stored form: a proposal adding a looser plans block to a document
// without one widens.
func TestWidensJSONPlansBlock(t *testing.T) {
	base, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}
	loose := StaffedProfile()
	loose.Plans = PlanCaps{MaxNewIndexesPerTable: 10, MaxNewIndexBytesPct: 50}
	next, err := MarshalDocument(loose)
	if err != nil {
		t.Fatalf("marshal next: %v", err)
	}
	widens, err := WidensJSON(base, next)
	if err != nil || !widens {
		t.Fatalf("looser plans block: (%v, %v), want widening", widens, err)
	}
	if widens, err := WidensJSON(next, base); err != nil || widens {
		t.Fatalf("back to the default caps: (%v, %v), want no widening", widens, err)
	}
}
