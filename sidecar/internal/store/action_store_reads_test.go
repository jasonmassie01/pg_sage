package store

import (
	"fmt"
	"testing"
)

func TestGroupLedgerByFinding(t *testing.T) {
	// No concurrent access test: grouping is a pure function of its input.
	actions := []QueuedAction{
		{ID: 3, FindingID: 10}, {ID: 2, FindingID: 10}, {ID: 9, FindingID: 11},
	}
	got := groupLedgerByFinding(actions, 2)
	if len(got) != 2 || len(got[10]) != 2 || len(got[11]) != 1 {
		t.Fatalf("grouped = %v", got)
	}
	if got[10][0].ID != 3 || got[10][1].ID != 2 || got[11][0].ID != 9 {
		t.Fatalf("order not kept: %s", fmt.Sprint(got))
	}
	if empty := groupLedgerByFinding(nil, 0); empty == nil || len(empty) != 0 {
		t.Fatalf("groupLedgerByFinding(nil) = %v, want empty map", empty)
	}
}
