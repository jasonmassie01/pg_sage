package changeplan

import "sync"

// pg_sage_plan_approvals_total{mode} (ux-03): new plan approvals by the
// mode stored (all, up_to, step), counted once each when the approval
// commits, whatever the channel (dashboard, API, CLI, chat). The share
// approved once (all and up_to) against step by step is how much
// multi-step work needs a person per step. The process keeps the counts
// in memory; a restart starts them at zero, as a counter may.

var approvalCounts = struct {
	sync.Mutex
	byMode map[Mode]uint64
}{byMode: map[Mode]uint64{}}

var countedModes = []Mode{ModeAll, ModeUpTo, ModeStep}

// countApproval counts one new approval of mode; another mode is not
// counted, so no label value is minted.
func countApproval(mode Mode) {
	switch mode {
	case ModeAll, ModeUpTo, ModeStep:
	default:
		return
	}
	approvalCounts.Lock()
	defer approvalCounts.Unlock()
	approvalCounts.byMode[mode]++
}

// ApprovalCounts is a copy of the counts with every mode present.
func ApprovalCounts() map[Mode]uint64 {
	approvalCounts.Lock()
	defer approvalCounts.Unlock()
	out := make(map[Mode]uint64, len(countedModes))
	for _, m := range countedModes {
		out[m] = approvalCounts.byMode[m]
	}
	return out
}

// resetApprovalCounts forgets every count (tests).
func resetApprovalCounts() {
	approvalCounts.Lock()
	defer approvalCounts.Unlock()
	approvalCounts.byMode = map[Mode]uint64{}
}
