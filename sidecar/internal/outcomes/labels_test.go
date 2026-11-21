package outcomes

import (
	"sort"
	"testing"
)

// pub-01: LabelledActionTypes is the set Label words itself, which the
// public bundle names; every other type is published as "other".

func TestLabelledActionTypes_AreSortedAndEachIsWorded(t *testing.T) {
	got := LabelledActionTypes()
	if len(got) != len(labels) || !sort.StringsAreSorted(got) {
		t.Fatalf("labelled types %v, want the %d labels sorted", got, len(labels))
	}
	for _, typ := range got {
		if Label(typ) != labels[typ] {
			t.Errorf("%s is listed but Label words it %q", typ, Label(typ))
		}
	}
	got[0] = "mutated"
	if LabelledActionTypes()[0] == "mutated" {
		t.Fatal("the list is shared with the caller")
	}
}
