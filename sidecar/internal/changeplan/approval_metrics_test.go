package changeplan

import (
	"sync"
	"testing"
)

// ux-03: pg_sage_plan_approvals_total{mode} counts new plan approvals by
// the mode stored, so the share approved once (all, up_to) against step by
// step can be read. No database here: approval_metrics_db_test.go counts
// through Store.ApproveWith.

func TestApprovalCounts_StartAtZeroForEveryMode(t *testing.T) {
	resetApprovalCounts()
	got := ApprovalCounts()
	if len(got) != 3 || got[ModeAll] != 0 || got[ModeUpTo] != 0 || got[ModeStep] != 0 {
		t.Fatalf("ApprovalCounts = %v, want all, up_to and step at 0", got)
	}
}

func TestApprovalCounts_CountEachModeAndIgnoreUnknown(t *testing.T) {
	resetApprovalCounts()
	countApproval(ModeAll)
	countApproval(ModeAll)
	countApproval(ModeUpTo)
	countApproval(ModeStep)
	countApproval(Mode("bogus"))
	countApproval("")
	got := ApprovalCounts()
	want := map[Mode]uint64{ModeAll: 2, ModeUpTo: 1, ModeStep: 1}
	if len(got) != len(want) {
		t.Fatalf("ApprovalCounts = %v, want %v (no label minted)", got, want)
	}
	for m, n := range want {
		if got[m] != n {
			t.Fatalf("ApprovalCounts[%s] = %d, want %d (all %v)", m, got[m], n, got)
		}
	}
}

func TestApprovalCounts_IsACopy(t *testing.T) {
	resetApprovalCounts()
	got := ApprovalCounts()
	got[ModeAll] = 99
	if ApprovalCounts()[ModeAll] != 0 {
		t.Fatal("changing the returned map changed the counter")
	}
}

func TestApprovalCounts_ConcurrentCountsAreNotLost(t *testing.T) {
	resetApprovalCounts()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			countApproval(ModeUpTo)
			_ = ApprovalCounts()
		}()
	}
	wg.Wait()
	if got := ApprovalCounts()[ModeUpTo]; got != 50 {
		t.Fatalf("up_to = %d after 50 concurrent counts, want 50", got)
	}
}
