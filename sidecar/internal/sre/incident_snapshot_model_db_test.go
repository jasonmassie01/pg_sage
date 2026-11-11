package sre

import (
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-05 on the model path: a detection-time root is never conclusive,
// whoever proposes it. Under earned root authority the investigator may
// root an inconclusive graph on an open hypothesis, but only with a
// verified claim citing live evidence: claims citing nothing but the
// detection snapshot are uncited, so the investigation stays
// inconclusive. A claim that also cites a live read is adopted as today.

// No concurrent access tests: one investigation runs per lease (see
// incident_snapshot_db_test.go).

func TestIncidentSnapshot_ModelRootOnTheSnapshotAloneIsNotAdopted(t *testing.T) {
	for _, live := range []bool{false, true} {
		st, pool, ctx := liveStore(t, budgetLimits())
		id := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 20*time.Second)
		auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "earned"}}
		m := newFakeModel(t, submit(func(body string) invFinal {
			cited := []string{aliasOf(t, body, IncidentSnapshotProbeID, "ok")}
			if live {
				cited = append(cited, aliasOf(t, body, probes.LockGraph, "empty"))
			}
			return invFinal{Outcome: "conclude", Root: "idle_in_tx_holder",
				Claims: []invClaim{{Text: "At detection pid 4242 was idle in transaction.",
					EvidenceIDs: cited}}}
		}))
		c, _ := investigatorCoordinator(t, ctx, st, clearedChainRunner(), m.client(),
			invOptions{auth: auth})
		c.incidents = NewPGTriggerSource(pool, snapDB)
		inv := startAndRun(t, ctx, c, lockTrigger(id))
		if len(snapshotRows(t, ctx, st, inv)) != 1 {
			t.Fatalf("live=%v: want one committed snapshot", live)
		}
		mc := inv.Summary.ModelConclusion
		if mc == nil {
			t.Fatalf("live=%v: no model conclusion recorded", live)
		}
		label := "live=" + strconv.FormatBool(live)
		if !live {
			if inv.State != StateInconclusive || inv.Summary.Root != "" ||
				inv.Summary.Conclusive || mc.Outcome != ModelInconclusive ||
				mc.Authority == ContestAdopted {
				t.Fatalf("%s: %s root %q conclusion %+v, want an inconclusive, unadopted "+
					"model root", label, inv.State, inv.Summary.Root, mc)
			}
			idle := hypothesisNamed(t, ctx, st, inv, "idle_in_tx_holder")
			if idle == nil || idle.Status != HypothesisContributing {
				t.Fatalf("%s: idle hypothesis = %+v, want contributing", label, idle)
			}
			continue
		}
		if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
			mc.Outcome != ModelConcluded || mc.Authority != ContestAdopted {
			t.Fatalf("%s: %s root %q conclusion %+v, want the cited model root adopted",
				label, inv.State, inv.Summary.Root, mc)
		}
	}
}

// liveClaims counts only claims citing at least one non-snapshot row.
func TestLiveClaims_IgnoresClaimsCitingOnlyTheSnapshot(t *testing.T) {
	snap, graph := NewUUID(), NewUUID()
	stored := []Evidence{{ID: snap, ProbeID: string(IncidentSnapshotProbeID)},
		{ID: graph, ProbeID: string(probes.LockGraph)}}
	n := &Narrative{Claims: []NarrativeClaim{
		{EvidenceIDs: []UUID{snap}},
		{EvidenceIDs: []UUID{snap, graph}},
		{EvidenceIDs: []UUID{graph}},
	}}
	if got := liveClaims(n, stored); got != 2 {
		t.Fatalf("liveClaims = %d, want 2 (the snapshot-only claim left out)", got)
	}
	if got := liveClaims(nil, stored); got != 0 {
		t.Fatalf("liveClaims(nil) = %d, want 0", got)
	}
	only := &Narrative{Claims: []NarrativeClaim{{EvidenceIDs: []UUID{snap}}}}
	if got := liveClaims(only, stored); got != 0 {
		t.Fatalf("snapshot-only narrative: liveClaims = %d, want 0", got)
	}
	if got := liveClaims(only, nil); got != 1 {
		t.Fatalf("no stored evidence to name a snapshot: liveClaims = %d, want 1", got)
	}
}
