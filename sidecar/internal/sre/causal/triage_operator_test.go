package causal

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Operator triage (roadmap 2.1, "Investigate now"): the same mechanisms
// as an SLO burn triage, under its own family and with a reason that
// does not claim an SLO is burning.

func TestDiagnoseOperator_FindsTheDatabaseMechanism(t *testing.T) {
	d := DiagnoseOperator(idleChain("idle in transaction", 75), "checkout is slow")
	if d.Family != FamilyOperator || d.Subject != "checkout is slow" || !d.Conclusive ||
		d.Root == nil || d.Root.Node != IdleInTxHolder || d.GraphVersion != GraphVersion {
		t.Fatalf("diagnosis = %+v", d)
	}
}

func TestDiagnoseOperator_NoMechanismIsInconclusiveWithoutABurnClaim(t *testing.T) {
	o := []Observation{obs("L1", probes.LockGraph), obs("P1", probes.PreparedXacts),
		connObs("C1", t0, 10, group{app: "api", state: "active", n: 5}),
		obs("Q1", probes.PlanRegressions)}
	d := DiagnoseOperator(o, "checkout is slow")
	if d.Conclusive || d.Root != nil || d.Reason == "" ||
		strings.Contains(d.Reason, "burning") || !strings.Contains(d.Reason, "checkout is slow") {
		t.Fatalf("diagnosis = %+v", d)
	}
	if none := DiagnoseOperator(nil, ""); none.Conclusive || len(none.Missing) == 0 ||
		none.Family != FamilyOperator {
		t.Fatalf("no evidence at all: %+v", none)
	}
}

func TestDiagnoseOperator_MatchesTheSLOTriageMechanisms(t *testing.T) {
	o := idleChain("idle in transaction", 75)
	op, slo := DiagnoseOperator(o, "x"), DiagnoseSLOBurn(o, "x")
	if op.Root.Node != slo.Root.Node || len(op.Alternatives) != len(slo.Alternatives) ||
		len(op.RuledOut) != len(slo.RuledOut) {
		t.Fatalf("operator triage %+v differs from SLO triage %+v", op, slo)
	}
}
