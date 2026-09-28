package sre

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// M2 store: the diagnosis (hypotheses, summary) persists under the lease,
// every change appends to a verifiable hash chain, lists are scoped, and
// evidence references outside the investigation are rejected (CHECK-09).

// evaluating creates an investigation, claims it and commits one step of
// two probe results so it is ready to conclude.
func evaluating(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	subject string) (Lease, []Evidence) {
	t.Helper()
	req := lockStart(scope, subject)
	req.IncidentID, req.Actor = "inc-"+subject, "trigger:rca"
	inv, _, err := st.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lease, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease, step("step-1", StateEvaluating,
		probeResult(probes.LockGraph, probes.Row{"pid": int64(7)}),
		probeResult(probes.PreparedXacts))); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil || len(ev) != 2 {
		t.Fatalf("evidence = %d (%v), want 2", len(ev), err)
	}
	return lease, ev
}

func rootHypothesis(ev []Evidence) HypothesisRecord {
	return HypothesisRecord{GraphVersion: "causal-v2", Family: "lock_blocking",
		Node: "idle_in_tx_holder", Label: "idle-in-transaction holder",
		Mechanism: "holds locks", Subject: "pid 7", Status: HypothesisRoot,
		Confidence: 0.85, RefutationProbe: "lock_graph",
		Support: []Fact{{EvidenceID: ev[0].ID, Text: "root blocker pid 7 is idle"}}}
}

func ruledOutHypothesis(ev []Evidence) HypothesisRecord {
	return HypothesisRecord{GraphVersion: "causal-v2", Family: "lock_blocking",
		Node: "prepared_xact_holder", Label: "prepared-transaction holder",
		Mechanism: "2PC", Subject: "pid 7", Status: HypothesisRuledOut,
		RefutationProbe: "prepared_xacts",
		Contradict:      []Fact{{EvidenceID: ev[1].ID, Text: "no prepared transactions"}}}
}

func concluded(ev []Evidence) Conclusion {
	return Conclusion{State: StateConcluded,
		Summary: Summary{Family: "lock_blocking", GraphVersion: "causal-v2",
			Subject: "pid 7", Conclusive: true, Root: "idle_in_tx_holder",
			Missing: []MissingEvidence{{ProbeID: "long_transactions",
				Status: "no_privilege", Reason: "permission denied"}}},
		Hypotheses: []HypothesisRecord{rootHypothesis(ev), ruledOutHypothesis(ev)}}
}

func TestStore_ConcludePersistsDiagnosisAndChain(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 7")
	inv, err := st.Conclude(ctx, lease, concluded(ev))
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if inv.State != StateConcluded || inv.LeaseOwner != "" || inv.ConcludedAt.IsZero() ||
		inv.Summary.Root != "idle_in_tx_holder" || !inv.Summary.Conclusive ||
		len(inv.Summary.Missing) != 1 || inv.IncidentID != "inc-pid 7" ||
		inv.Subject != "pid 7" {
		t.Fatalf("concluded investigation = %+v", inv)
	}
	hs, err := st.Hypotheses(ctx, scope, lease.InvestigationID)
	if err != nil || len(hs) != 2 {
		t.Fatalf("hypotheses = %+v (%v)", hs, err)
	}
	if hs[0].Status != HypothesisRoot || hs[0].Revision != 1 || hs[0].Ordinal != 1 ||
		hs[0].Support[0].EvidenceID != ev[0].ID || hs[0].Confidence != 0.85 ||
		hs[1].Status != HypothesisRuledOut || hs[1].Contradict[0].EvidenceID != ev[1].ID {
		t.Fatalf("hypotheses = %+v", hs)
	}
	events, err := st.Events(ctx, scope, lease.InvestigationID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "created,claimed,step,concluded" {
		t.Fatalf("event types = %s", got)
	}
	if events[0].PreviousHash != nil || len(events[1].PreviousHash) != 32 ||
		string(events[1].PreviousHash) != string(events[0].Hash) ||
		events[0].Actor != "trigger:rca" {
		t.Fatalf("chain links = %+v", events[:2])
	}
	if err := st.VerifyEvents(ctx, scope, lease.InvestigationID); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// CHECK-09: a hypothesis citing evidence of another investigation (here
// in another database scope) is rejected and nothing is persisted.
func TestStore_ConcludeRejectsForeignEvidence(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 8")
	_, foreign := evaluating(t, ctx, st, testScope(t, ctx, st), "pid 8")
	c := concluded(ev)
	c.Hypotheses[0].Support = append(c.Hypotheses[0].Support,
		Fact{EvidenceID: foreign[0].ID, Text: "other database"})
	if _, err := st.Conclude(ctx, lease, c); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("conclude with foreign evidence = %v, want ErrInvalidRequest", err)
	}
	inv, _ := st.Get(ctx, scope, lease.InvestigationID)
	hs, _ := st.Hypotheses(ctx, scope, lease.InvestigationID)
	if inv.State != StateEvaluating || len(hs) != 0 {
		t.Fatalf("after rejected conclude: state %s, %d hypotheses", inv.State, len(hs))
	}
	c = concluded(ev)
	c.Hypotheses[1].Contradict[0].EvidenceID = "not-a-uuid"
	if _, err := st.Conclude(ctx, lease, c); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("conclude with a malformed evidence id = %v", err)
	}
}

func TestStore_ConcludeValidatesTheConclusion(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 9")
	bad := map[string]func(*Conclusion){
		"concluded without a root": func(c *Conclusion) { c.Hypotheses = c.Hypotheses[1:] },
		"two roots": func(c *Conclusion) {
			c.Hypotheses = append(c.Hypotheses, rootHypothesis(ev))
		},
		"unknown status":    func(c *Conclusion) { c.Hypotheses[0].Status = "likely" },
		"confidence over 1": func(c *Conclusion) { c.Hypotheses[0].Confidence = 1.5 },
		"no refutation":     func(c *Conclusion) { c.Hypotheses[0].RefutationProbe = "" },
		"ruled out without contradiction": func(c *Conclusion) {
			c.Hypotheses[1].Contradict = nil
		},
		"fact without evidence": func(c *Conclusion) {
			c.Hypotheses[0].Support[0].EvidenceID = ""
		},
		"not a final state": func(c *Conclusion) { c.State = StateCollecting },
		"failed without code": func(c *Conclusion) {
			c.State, c.Hypotheses = StateFailed, nil
		},
	}
	for name, mutate := range bad {
		c := concluded(ev)
		mutate(&c)
		if _, err := st.Conclude(ctx, lease, c); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: conclude = %v, want ErrInvalidRequest", name, err)
		}
	}
	// Inconclusive with no hypotheses at all is a valid, first-class outcome.
	inv, err := st.Conclude(ctx, lease, Conclusion{State: StateInconclusive,
		Summary: Summary{Family: "lock_blocking", Reason: "no lock waits at probe time"}})
	if err != nil || inv.State != StateInconclusive || inv.Summary.Reason == "" {
		t.Fatalf("inconclusive = %+v (%v)", inv, err)
	}
}

// A stale worker (its lease expired and was re-claimed) cannot conclude,
// and a zero lease is rejected before any I/O.
func TestStore_ConcludeNeedsTheCurrentLease(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 300 * time.Millisecond
	st, _, ctx := liveStore(t, limits)
	scope := testScope(t, ctx, st)
	stale, ev := evaluating(t, ctx, st, scope, "pid 10")
	time.Sleep(time.Until(stale.Until) + 300*time.Millisecond)
	fresh, err := st.Claim(ctx, scope, stale.InvestigationID, NewUUID())
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if _, err := st.Conclude(ctx, stale, concluded(ev)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale conclude = %v, want ErrLeaseLost", err)
	}
	if _, err := st.Conclude(ctx, Lease{}, concluded(ev)); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("zero lease = %v", err)
	}
	if _, err := st.Conclude(ctx, fresh, concluded(ev)); err != nil {
		t.Fatalf("current worker conclude: %v", err)
	}
}

func TestStore_EventChainIsAppendOnlyAndDetectsTampering(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 11")
	if _, err := st.Conclude(ctx, lease, concluded(ev)); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	id := string(lease.InvestigationID)
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_events SET actor = 'mallory'
		WHERE investigation_id = $1`, id); err == nil {
		t.Fatal("UPDATE of sre_events succeeded; events must be append-only")
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_evidence SET payload = '{}'
		WHERE investigation_id = $1`, id); err == nil {
		t.Fatal("UPDATE of sre_evidence succeeded; evidence must be append-only")
	}
	// A database administrator can still bypass the trigger; the chain
	// then fails verification.
	if _, err := pool.Exec(ctx, `DELETE FROM sage.sre_events
		WHERE investigation_id = $1 AND sequence = 2`, id); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if err := st.VerifyEvents(ctx, scope, lease.InvestigationID); !errors.Is(err,
		ErrChainBroken) {
		t.Fatalf("verify after a removed link = %v, want ErrChainBroken", err)
	}
}

func TestStore_ListIsScopedAndPaged(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	a, b := testScope(t, ctx, st), testScope(t, ctx, st)
	for _, subject := range []string{"pid 1", "pid 2", "pid 3"} {
		if _, _, err := st.Create(ctx, lockStart(a, subject)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	other, _, _ := st.Create(ctx, lockStart(b, "pid 1"))
	first, err := st.List(ctx, a, ListFilter{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" ||
		first.Items[0].Subject != "pid 3" {
		t.Fatalf("first page = %+v (%v)", first, err)
	}
	second, err := st.List(ctx, a, ListFilter{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" ||
		second.Items[0].Subject != "pid 1" {
		t.Fatalf("second page = %+v (%v)", second, err)
	}
	onlyB, _ := st.List(ctx, b, ListFilter{})
	if len(onlyB.Items) != 1 || onlyB.Items[0].ID != other.ID {
		t.Fatalf("scope b lists %+v", onlyB.Items)
	}
	for _, f := range []ListFilter{{Limit: -1}, {Limit: 501}, {Cursor: "garbage"}} {
		if _, err := st.List(ctx, a, f); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("list %+v = %v, want ErrInvalidRequest", f, err)
		}
	}
	if _, err := st.Get(ctx, b, first.Items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get across scopes = %v, want ErrNotFound", err)
	}
}

func TestStore_PinAndEvidenceByID(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 12")
	inv, err := st.SetPinned(ctx, scope, lease.InvestigationID, true, "user:4")
	if err != nil || !inv.Pinned {
		t.Fatalf("pin = %+v (%v)", inv, err)
	}
	if inv, err = st.SetPinned(ctx, scope, lease.InvestigationID, false,
		"user:4"); err != nil || inv.Pinned {
		t.Fatalf("unpin = %+v (%v)", inv, err)
	}
	if _, err := st.SetPinned(ctx, scope, NewUUID(), true, "user:4"); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("pin unknown = %v", err)
	}
	if _, err := st.SetPinned(ctx, scope, lease.InvestigationID, true, ""); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("pin without actor = %v", err)
	}
	events, _ := st.Events(ctx, scope, lease.InvestigationID)
	last := events[len(events)-1]
	var payload map[string]any
	_ = json.Unmarshal(last.Payload, &payload)
	if last.Type != EventUnpinned || last.Actor != "user:4" {
		t.Fatalf("last event = %+v", last)
	}
	got, err := st.EvidenceByID(ctx, scope, lease.InvestigationID, ev[1].ID)
	if err != nil || got.ID != ev[1].ID || !got.VerifyHash() {
		t.Fatalf("evidence by id = %+v (%v)", got, err)
	}
	_, other := evaluating(t, ctx, st, scope, "pid 13")
	if _, err := st.EvidenceByID(ctx, scope, lease.InvestigationID,
		other[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evidence of another investigation = %v, want ErrNotFound", err)
	}
}
