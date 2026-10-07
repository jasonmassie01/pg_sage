package perfgate

import (
	"strings"
	"testing"
)

// A table whose only update is a one-time state transition between the
// partial indexes it is read through cannot write HOT updates by design:
// it is exempt from gate F, by name and with the reason. Every other
// table is still charged.

func hotPhase(tables ...TableDelta) Phase {
	p := steadyPhase()
	p.Tables = tables
	return p
}

func TestHotGateExemptsOnlyNamedTransitionTables(t *testing.T) {
	b := DefaultBudgets()
	if len(b.HotExempt) == 0 {
		t.Fatal("no HOT exemptions: shadow_decision's scoring cannot be HOT")
	}
	reason, ok := b.HotExempt["sage.shadow_decision"]
	if !ok || !strings.Contains(reason, "pending") || !strings.Contains(reason, "partial") {
		t.Fatalf("shadow_decision exemption = %q %t, want the pending-to-scored reason",
			reason, ok)
	}
	for table, why := range b.HotExempt {
		if !strings.HasPrefix(table, "sage.") || len(why) < 40 {
			t.Fatalf("exemption %q = %q: a sage table with a reason, please", table, why)
		}
	}
	got, err := Evaluate([]Phase{hotPhase(
		TableDelta{Name: "sage.shadow_decision", Updates: 31, HotUpdates: 0},
		TableDelta{Name: "sage.findings", Updates: 31, HotUpdates: 0},
	)}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateHotUpdates || got[0].Subject != "sage.findings" {
		t.Fatalf("offenders = %+v, want only sage.findings", got)
	}
}

// The exemption is a budget, not a hard-coded skip: without it the table
// is charged like any other.
func TestHotGateChargesAnExemptTableWhenTheExemptionIsRemoved(t *testing.T) {
	b := DefaultBudgets()
	b.HotExempt = nil
	got, err := Evaluate([]Phase{hotPhase(
		TableDelta{Name: "sage.shadow_decision", Updates: 31, HotUpdates: 0})}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Subject != "sage.shadow_decision" {
		t.Fatalf("offenders = %+v, want shadow_decision charged", got)
	}
}

// The report says which tables were exempt and why, so a reader sees the
// gate's judgment rather than a silent pass.
func TestReportListsHotExemptions(t *testing.T) {
	b := DefaultBudgets()
	md := RenderMarkdown(Scale{}, b, []Phase{hotPhase(
		TableDelta{Name: "sage.shadow_decision", Updates: 31, HotUpdates: 0})}, nil)
	if !strings.Contains(md, "sage.shadow_decision") ||
		!strings.Contains(md, b.HotExempt["sage.shadow_decision"]) {
		t.Fatalf("report omits the HOT exemption and its reason:\n%s", md)
	}
}
