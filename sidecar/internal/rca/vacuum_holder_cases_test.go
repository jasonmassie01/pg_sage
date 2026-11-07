package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01 review: Cases routes a vacuum_blocked root cause reading
// "Idle-in-transaction PID" to the idle-in-transaction playbook, so that
// text must follow the tree's terminate guard; since playbooks-01 the
// playbook's signals come only from structured blocker identities.

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

// The holder the tree terminates keeps that terminate as the incident's
// recommended SQL, but Cases builds no signal from it: since playbooks-01 a
// backend signal comes only from a structured lock_contention blocker
// identity, never from incident text, so the idle-in-transaction playbook
// offers only its blocked cancel and no terminate candidate.
func TestVacuumBlockedHolder_CasesOfferNoTextSignalForTheTreesTerminate(t *testing.T) {
	idleTx := "idle in transaction"
	inc := blockedHolder(t, []probes.XminHolder{sessionAt(911, 800, time.Hour, idleTx)},
		sessionLock(911, time.Hour, time.Hour, idleTx))
	if !strings.HasPrefix(inc.RecommendedSQL, "SELECT pg_terminate_backend(911);") {
		t.Fatalf("recommended SQL = %q, want the tree's terminate of 911", inc.RecommendedSQL)
	}
	got := backendActions(inc)
	if len(got) != 1 || got["cancel_backend"] != "" {
		t.Fatalf("cases candidates %v for %q, want only the blocked cancel (no SQL)",
			got, inc.RootCause)
	}
}
