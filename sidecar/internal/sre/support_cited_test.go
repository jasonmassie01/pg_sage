package sre

import (
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The Support-evidence rule (sre-03 Phase 2): a model contest or
// conclusion needs at least one kept claim citing evidence the causal
// graph records as support for the model's root. A claim about anything
// else (connection counts under a lock contest) does not carry it.

// No concurrent-access tests: supportCited is a pure function over its
// inputs and holds no shared state.

// supportFixture is a conclusive lock diagnosis whose root, an
// alternative and a ruled-out hypothesis each have their own support
// evidence, plus the stored rows those ids name.
type supportFixture struct {
	d                           causal.Diagnosis
	stored                      []Evidence
	rootEv, ddlEv, connEv, snap UUID
	ruledOutEv, unstoredSupport UUID
}

func newSupportFixture() supportFixture {
	f := supportFixture{rootEv: NewUUID(), ddlEv: NewUUID(), connEv: NewUUID(),
		snap: NewUUID(), ruledOutEv: NewUUID(), unstoredSupport: NewUUID()}
	fact := func(id UUID) causal.Fact { return causal.Fact{EvidenceID: string(id), Text: "x"} }
	root := hyp(causal.IdleInTxHolder, causal.StatusRoot, 0.9)
	root.Support = []causal.Fact{fact(f.rootEv), fact(f.snap)}
	ddl := hyp(causal.DDLLockQueue, causal.StatusContributing, 0.6)
	ddl.Support = []causal.Fact{fact(f.ddlEv), fact(f.unstoredSupport)}
	conn := hyp(causal.HotRowContention, causal.StatusAlternative, 0.3)
	conn.Support = []causal.Fact{fact(f.connEv)}
	ruled := hyp(causal.PreparedXactHolder, causal.StatusRuledOut, 0)
	ruled.Support = []causal.Fact{fact(f.ruledOutEv)}
	f.d = causal.Diagnosis{Family: causal.FamilyLockBlocking, Conclusive: true, Root: &root,
		Contributing: []causal.Hypothesis{ddl}, Alternatives: []causal.Hypothesis{conn},
		RuledOut: []causal.Hypothesis{ruled}}
	f.stored = []Evidence{{ID: f.rootEv, ProbeID: string(probes.LockGraph)},
		{ID: f.ddlEv, ProbeID: string(probes.LockGraph)},
		{ID: f.connEv, ProbeID: string(probes.LongTransactions)},
		{ID: f.ruledOutEv, ProbeID: string(probes.PreparedXacts)},
		{ID: f.snap, ProbeID: string(IncidentSnapshotProbeID)}}
	return f
}

func claims(ids ...[]UUID) *Narrative {
	n := &Narrative{Label: NarrativeLabel}
	for _, c := range ids {
		n.Claims = append(n.Claims, NarrativeClaim{Text: "claim", EvidenceIDs: c})
	}
	return n
}

func TestSupportCited_ClaimCitingTheRootsSupportCounts(t *testing.T) {
	f := newSupportFixture()
	cases := []struct {
		name string
		root string
		n    *Narrative
		want bool
	}{
		{"contributing root supported", "ddl_lock_queue", claims([]UUID{f.ddlEv}), true},
		{"alternative root supported", "hot_row_contention", claims([]UUID{f.connEv}), true},
		{"graph root supported", "idle_in_tx_holder", claims([]UUID{f.rootEv}), true},
		{"ruled-out root still names its support", "prepared_xact_holder",
			claims([]UUID{f.ruledOutEv}), true},
		{"claim about another hypothesis", "ddl_lock_queue", claims([]UUID{f.connEv}), false},
		{"claim about the graph root under a contest", "ddl_lock_queue",
			claims([]UUID{f.rootEv}), false},
		{"one of several claims supports", "ddl_lock_queue",
			claims([]UUID{f.connEv}, []UUID{f.ddlEv}), true},
	}
	for _, tc := range cases {
		if got := supportCited(f.d, tc.root, tc.n, f.stored); got != tc.want {
			t.Errorf("%s: supportCited(%s) = %v, want %v", tc.name, tc.root, got, tc.want)
		}
	}
}

func TestSupportCited_SecondCitedIDOfAClaimCounts(t *testing.T) {
	f := newSupportFixture()
	if !supportCited(f.d, "ddl_lock_queue", claims([]UUID{f.connEv, f.ddlEv}), f.stored) {
		t.Fatal("a claim whose second cited id supports the root did not count")
	}
	if !supportCited(f.d, "ddl_lock_queue", claims([]UUID{f.ddlEv, f.connEv}), f.stored) {
		t.Fatal("a claim whose first cited id supports the root did not count")
	}
}

func TestSupportCited_SnapshotEvidenceNeverCounts(t *testing.T) {
	f := newSupportFixture()
	// The snapshot is support of the graph root, yet like liveClaims a
	// detection-time row never carries a model root.
	if supportCited(f.d, "idle_in_tx_holder", claims([]UUID{f.snap}), f.stored) {
		t.Fatal("a claim citing only the detection snapshot counted as support")
	}
	if !supportCited(f.d, "idle_in_tx_holder", claims([]UUID{f.snap, f.rootEv}), f.stored) {
		t.Fatal("a live support id beside the snapshot did not count")
	}
}

func TestSupportCited_EvidenceNotStoredCountsForNothing(t *testing.T) {
	f := newSupportFixture()
	// unstoredSupport is in the hypothesis' Support but not stored: the
	// claim cannot rest on evidence the investigation does not hold.
	if supportCited(f.d, "ddl_lock_queue", claims([]UUID{f.unstoredSupport}), f.stored) {
		t.Fatal("a claim citing support evidence that is not stored counted")
	}
	if supportCited(f.d, "ddl_lock_queue", claims([]UUID{NewUUID()}), f.stored) {
		t.Fatal("a claim citing an unknown evidence id counted")
	}
}

func TestSupportCited_UnknownOrEmptyRootIsFalse(t *testing.T) {
	f := newSupportFixture()
	for _, root := range []string{"", "cosmic_rays", "connection_leak"} {
		if supportCited(f.d, root, claims([]UUID{f.ddlEv, f.connEv, f.rootEv}), f.stored) {
			t.Errorf("root %q with no hypothesis counted as supported", root)
		}
	}
}

func TestSupportCited_NilAndEmptyInputsAreFalse(t *testing.T) {
	f := newSupportFixture()
	if supportCited(f.d, "ddl_lock_queue", nil, f.stored) {
		t.Error("nil narrative counted as supported")
	}
	if supportCited(f.d, "ddl_lock_queue", &Narrative{}, f.stored) {
		t.Error("narrative without claims counted as supported")
	}
	if supportCited(f.d, "ddl_lock_queue", claims([]UUID{}), f.stored) {
		t.Error("claim without evidence ids counted as supported")
	}
	if supportCited(f.d, "ddl_lock_queue", claims([]UUID{f.ddlEv}), nil) {
		t.Error("empty stored evidence counted as supported")
	}
	if supportCited(causal.Diagnosis{}, "ddl_lock_queue", claims([]UUID{f.ddlEv}),
		f.stored) {
		t.Error("empty diagnosis counted as supported")
	}
	bare := f.d
	ddl := bare.Contributing[0]
	ddl.Support = nil
	bare.Contributing = []causal.Hypothesis{ddl}
	if supportCited(bare, "ddl_lock_queue", claims([]UUID{f.ddlEv}), f.stored) {
		t.Error("hypothesis with empty Support counted as supported")
	}
}

// The graph binds facts to evidence by the stored id's canonical string
// form (observations, plan.go): the rule matches the same form.
func TestSupportCited_MatchesTheObservationEvidenceIDFormat(t *testing.T) {
	id := NewUUID()
	payload, err := json.Marshal(probes.Result{ProbeID: probes.LockGraph,
		Status: probes.StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := observations([]Evidence{{ID: id, ProbeID: string(probes.LockGraph),
		Payload: payload}})
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v (%v)", obs, err)
	}
	if obs[0].EvidenceID != string(id) || len(obs[0].EvidenceID) != 36 {
		t.Fatalf("observation evidence id %q, want the canonical UUID %q",
			obs[0].EvidenceID, id)
	}
	h := hyp(causal.DDLLockQueue, causal.StatusAlternative, 0.5)
	h.Support = []causal.Fact{{EvidenceID: obs[0].EvidenceID}}
	d := causal.Diagnosis{Alternatives: []causal.Hypothesis{h}}
	if !supportCited(d, "ddl_lock_queue", claims([]UUID{id}),
		[]Evidence{{ID: id, ProbeID: string(probes.LockGraph)}}) {
		t.Fatal("the claim's UUID did not match the fact bound by observations")
	}
}
