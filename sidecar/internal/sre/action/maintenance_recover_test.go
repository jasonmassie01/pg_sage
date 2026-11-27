package action

import (
	"strings"
	"testing"
)

// No concurrent access tests here: settleStranded is a pure function of
// its arguments; the database tests race the sweeper against a late record.

func strandedProposal(state string) MaintenanceProposal {
	return MaintenanceProposal{State: state, Class: "create_statistics",
		Verification: "not_started", StatisticsOID: 0}
}

func TestSettleStrandedExecuting(t *testing.T) {
	cases := []struct {
		name      string
		obs       statisticsObservation
		state     string
		actionID  int64
		oid       uint32
		verify    string
		detailHas string
	}{
		{"action and object agree", statisticsObservation{actions: 1, actionID: 41,
			outcome: "monitoring", objectOID: 7001}, "uncertain", 41, 7001,
			"inconclusive", "typed rollback is available"},
		{"nothing ran", statisticsObservation{}, "failed", 0, 0, "failed",
			"no statistics object exists"},
		{"executor recorded a failure", statisticsObservation{actions: 1, actionID: 42,
			outcome: "failed"}, "failed", 42, 0, "failed", "recorded a failed run"},
		{"object without action record", statisticsObservation{objectOID: 7002},
			"uncertain", 0, 0, "inconclusive", "no action record"},
		{"action without object", statisticsObservation{actions: 1, actionID: 43,
			outcome: "monitoring"}, "uncertain", 43, 0, "inconclusive", "is absent"},
		{"name held on another table", statisticsObservation{foreign: true},
			"uncertain", 0, 0, "inconclusive", "another table"},
		{"failed action but object present", statisticsObservation{actions: 1,
			actionID: 44, outcome: "failed", objectOID: 7003}, "uncertain", 44, 0,
			"inconclusive", "recorded a failure"},
		{"several action records", statisticsObservation{actions: 2, objectOID: 7004},
			"uncertain", 0, 0, "inconclusive", "2 action records"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := strandedProposal("executing")
			typ := settleStranded(&p, c.obs)
			if typ != "action_failed" || p.State != c.state || p.ActionLogID != c.actionID ||
				p.StatisticsOID != c.oid || p.Verification != c.verify {
				t.Fatalf("got %s %+v", typ, p)
			}
			if !strings.Contains(p.Detail, c.detailHas) {
				t.Fatalf("detail %q lacks %q", p.Detail, c.detailHas)
			}
			if p.StatisticsOID == 0 && p.State == "uncertain" &&
				!strings.Contains(p.Detail, "rollback is refused") {
				t.Fatalf("unproven object must say rollback is refused: %q", p.Detail)
			}
		})
	}
}

func TestSettleStrandedRollingBack(t *testing.T) {
	cases := []struct {
		name      string
		obs       statisticsObservation
		typ       string
		state     string
		verify    string
		detailHas string
	}{
		{"object still present", statisticsObservation{objectOID: 9001},
			"action_failed", "rollback_failed", "failed", "retry the rollback"},
		{"object removed", statisticsObservation{}, "action_executed", "rolled_back",
			"object_removed", "cannot be restored"},
		{"replacement holds the name", statisticsObservation{objectOID: 9002},
			"action_executed", "rolled_back", "object_removed", "was not touched"},
		{"name held on another table", statisticsObservation{foreign: true},
			"action_executed", "rolled_back", "object_removed", "was not touched"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := strandedProposal("rolling_back")
			p.ActionLogID, p.StatisticsOID = 50, 9001
			typ := settleStranded(&p, c.obs)
			if typ != c.typ || p.State != c.state || p.Verification != c.verify {
				t.Fatalf("got %s %+v", typ, p)
			}
			if p.ActionLogID != 50 || p.StatisticsOID != 9001 {
				t.Fatalf("rollback reconciliation must keep the identity: %+v", p)
			}
			if !strings.Contains(p.Detail, c.detailHas) {
				t.Fatalf("detail %q lacks %q", p.Detail, c.detailHas)
			}
		})
	}
}

func TestSettleStrandedIgnoresSettledStates(t *testing.T) {
	for _, state := range []string{"", "proposed", "requested", "executed", "uncertain",
		"failed", "rolled_back", "rollback_failed"} {
		p := strandedProposal(state)
		p.ActionLogID, p.StatisticsOID, p.Detail = 3, 4, "kept"
		if typ := settleStranded(&p, statisticsObservation{actions: 1, objectOID: 4}); typ != "" {
			t.Fatalf("%q settled as %s", state, typ)
		}
		if p.State != state || p.ActionLogID != 3 || p.StatisticsOID != 4 || p.Detail != "kept" {
			t.Fatalf("%q was changed: %+v", state, p)
		}
	}
}
