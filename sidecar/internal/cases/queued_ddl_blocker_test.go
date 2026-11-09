package cases

import (
	"strings"
	"testing"
)

// migration-05: when the sessions queue behind an in-flight DDL that waits
// for the root blocker's lock, signalling the root blocker is the wrong
// remedy. The lock_contention incident keeps the blocker's evidence but
// offers no runnable cancel or terminate of it; the DDL's own
// migration_safety finding carries the approval-only cancel of the DDL.
func TestQueuedDDLRootBlockerGetsNoSignal(t *testing.T) {
	for _, state := range []string{stateActive, stateIdleInTx} {
		b := activeBlocker()
		b.State = state
		b.QueuedDDLPID = 200
		got := ProjectIncident(lockIncident(SeverityCritical, b))
		if hasCandidate(got, "terminate_backend") {
			t.Fatalf("%s: queued-DDL root blocker got a terminate: %+v", state,
				got.ActionCandidates)
		}
		c := candidateOf(t, got, "cancel_backend")
		if c.ProposedSQL != "" || !strings.Contains(c.BlockedReason, "DDL pid 200") {
			t.Fatalf("%s: cancel = SQL %q reason %q; want blocked, naming DDL pid 200",
				state, c.ProposedSQL, c.BlockedReason)
		}
	}
}

// Boundary: QueuedDDLPID zero is "no queued DDL", and the root blocker
// keeps its guarded cancel.
func TestZeroQueuedDDLPIDKeepsGuardedCancel(t *testing.T) {
	b := activeBlocker()
	b.QueuedDDLPID = 0
	c := candidateOf(t, ProjectIncident(lockIncident(SeverityWarning, b)), "cancel_backend")
	if c.BlockedReason != "" || !strings.Contains(c.ProposedSQL, "a.pid = 4242") {
		t.Fatalf("cancel = SQL %q reason %q, want the guarded script", c.ProposedSQL,
			c.BlockedReason)
	}
}
