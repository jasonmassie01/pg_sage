package rca

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01 review: Cases builds cancel and terminate candidates from a
// vacuum_blocked root cause reading "Idle-in-transaction PID", so the
// tree's terminate guard holds only if that text follows it.

// No concurrent access test: the projection is a pure function of one
// incident.

func backendActions(inc Incident) map[string]string {
	src := cases.SourceIncident{ID: "inc-1", DatabaseName: "orders",
		Severity: cases.Severity(inc.Severity), RootCause: inc.RootCause,
		AffectedObjects: inc.AffectedObjects, SignalIDs: inc.SignalIDs,
		RecommendedSQL: inc.RecommendedSQL, ActionRisk: inc.ActionRisk, Source: "rca",
		DetectedAt: holderNow, LastDetectedAt: holderNow}
	for _, l := range inc.CausalChain {
		src.CausalChain = append(src.CausalChain, cases.IncidentChainLink{Order: l.Order,
			Signal: l.Signal, Description: l.Description, Evidence: l.Evidence})
	}
	out := map[string]string{}
	for _, c := range cases.ProjectIncident(src).ActionCandidates {
		if c.ActionType == "cancel_backend" || c.ActionType == "terminate_backend" {
			out[c.ActionType] = c.ProposedSQL
		}
	}
	return out
}

// A holder the tree will not signal (a protected application, idle under
// the terminate minutes, idle time unknown) gets no Cases signal either.
func TestVacuumBlockedHolder_CasesOfferNoSignalTheTreeWithheld(t *testing.T) {
	idleTx := "idle in transaction"
	dump := sessionAt(901, 800, time.Hour, idleTx)
	dump.Application = "pg_dump"
	for name, snap := range map[string]*collector.Snapshot{
		"pg_dump": holderSnapshot([]probes.XminHolder{dump},
			sessionLock(901, time.Hour, time.Hour, idleTx)),
		"briefly idle": holderSnapshot([]probes.XminHolder{
			sessionAt(902, 800, time.Hour, idleTx)},
			sessionLock(902, time.Hour, time.Minute, idleTx)),
		"idle time unknown": holderSnapshot([]probes.XminHolder{
			sessionAt(903, 800, time.Hour, idleTx)}),
	} {
		t.Run(name, func(t *testing.T) {
			inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, testVacuumRules())
			assertNoSignal(t, inc)
			if got := backendActions(inc); len(got) != 0 {
				t.Fatalf("cases candidates %v for %q, want none", got, inc.RootCause)
			}
		})
	}
}

// The holder the tree terminates still reaches Cases as a terminate of
// the same pid.
func TestVacuumBlockedHolder_CasesTerminateTheTreesHolder(t *testing.T) {
	idleTx := "idle in transaction"
	inc := blockedHolder(t, []probes.XminHolder{sessionAt(911, 800, time.Hour, idleTx)},
		sessionLock(911, time.Hour, time.Hour, idleTx))
	got := backendActions(inc)
	if got["terminate_backend"] != "SELECT pg_terminate_backend(911)" {
		t.Fatalf("cases candidates %v for %q, want the terminate of 911", got, inc.RootCause)
	}
}
