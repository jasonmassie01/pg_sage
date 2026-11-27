package approvalcard

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// policy-05: a revert item's card says pg_sage withheld the automatic
// revert and why; after an emergency stop, that it is approvable once the
// stop is lifted.
func TestRevertReasons(t *testing.T) {
	if got := revertReasons(nil); got != nil {
		t.Fatalf("no revert: %+v", got)
	}
	policyHeld := revertReasons(&RevertRow{ActionID: 42, Outcome: "rollback_skipped",
		Reason: "standing policy withheld automatic rollback (blocked: executor_disabled)"})
	if len(policyHeld) != 1 || policyHeld[0].Code != "revert_withheld" ||
		!strings.Contains(policyHeld[0].Text, "Action 42") ||
		!strings.Contains(policyHeld[0].Text, "executor_disabled") ||
		strings.Contains(policyHeld[0].Text, "stop is lifted") {
		t.Fatalf("policy refusal: %+v", policyHeld)
	}
	stopped := revertReasons(&RevertRow{ActionID: 7,
		Reason: "emergency stop active; automatic rollback withheld"})
	if len(stopped) != 1 || !strings.HasSuffix(stopped[0].Text,
		"approve the revert after the stop is lifted") {
		t.Fatalf("emergency stop: %+v", stopped)
	}
	empty := revertReasons(&RevertRow{ActionID: 9})
	if len(empty) != 1 || !strings.Contains(empty[0].Text, "no reason was recorded") {
		t.Fatalf("empty reason: %+v", empty)
	}
}

// A revert item's finding was acted on by the action it reverts: the card
// leads with the withheld revert and does not call the finding missing.
func TestWhyOfRevertItem(t *testing.T) {
	in := Inputs{Action: store.QueuedAction{ActionType: "revert_action"},
		Revert: &RevertRow{ActionID: 5, Reason: "standing policy withheld automatic " +
			"rollback (blocked: replica_mutation)"}}
	got := whyOf(in, "revert_created_index")
	if len(got) == 0 || got[0].Code != "revert_withheld" {
		t.Fatalf("why = %+v, want revert_withheld first", got)
	}
	for _, r := range got {
		if r.Code == "finding_missing" {
			t.Fatalf("a revert item's card called its finding missing: %+v", got)
		}
	}
	plain := whyOf(Inputs{Action: store.QueuedAction{}}, "create_index_concurrently")
	if plain[len(plain)-1].Code != "finding_missing" {
		t.Fatalf("an ordinary item without a finding: %+v", plain)
	}
}
