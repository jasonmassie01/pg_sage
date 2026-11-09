package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// policy-03: a decision carries the standing-policy version and document
// hash the gate evaluated, so the recorder never reads the policy a second
// time (a ratification between the two reads recorded the wrong version).
// A decision made before any read carries version 0 and no hash.

const provenanceSHA = "4f0c6c0a3c7b0c5b9a1f0e5d2c3b4a5968778695a4b3c2d1e0f1e2d3c4b5a697"

// provenanceDoc is an always-open unattended document stamped as the
// policy reader stamps a stored row.
func provenanceDoc(version int64) Document {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.Version, doc.SHA256 = version, provenanceSHA
	return doc
}

// provenanceGate records the decisions it is handed and counts policy reads.
type provenanceGate struct {
	reads    atomic.Int32
	mu       sync.Mutex
	recorded []Decision
}

func (p *provenanceGate) gate(runtime RuntimeState, read func(ActionRequest) (Document,
	error)) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return runtime, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(_ context.Context, req ActionRequest) (Document, error) {
			p.reads.Add(1)
			return read(req)
		},
		RecordDecisionDetailed: func(
			_ context.Context, _ ActionRequest, d Decision,
		) (string, int64, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.recorded = append(p.recorded, d)
			return "ev-provenance", int64(len(p.recorded)), nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC) },
	})
}

func provenanceRuntime() RuntimeState {
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	return RuntimeState{ExecutorEnabled: true, TrustLevel: TrustAutonomous,
		ExecutionMode: ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
		RampStart: now.Add(-60 * 24 * time.Hour), InConfiguredWindow: true}
}

func assertProvenance(t *testing.T, d Decision, version int64, sha string) {
	t.Helper()
	if d.PolicyVersion != version || d.PolicySHA256 != sha {
		t.Fatalf("decision %s carries policy v%d %q, want v%d %q", d.LedgerLabel(),
			d.PolicyVersion, d.PolicySHA256, version, sha)
	}
}

func TestDecisionCarriesTheEvaluatedPolicy(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
		return provenanceDoc(7), nil
	})
	d := gate.Authorize(context.Background(), validIndexRequest())
	if d.Verdict != VerdictExecute {
		t.Fatalf("verdict = %s (%s), want execute", d.Verdict, d.Detail)
	}
	assertProvenance(t, d, 7, provenanceSHA)
	if len(p.recorded) != 1 {
		t.Fatalf("recorded %d decisions, want 1", len(p.recorded))
	}
	assertProvenance(t, p.recorded[0], 7, provenanceSHA)
	if got := p.reads.Load(); got != 1 {
		t.Fatalf("policy read %d times for one decision, want 1", got)
	}
}

// A withheld verdict decided by the document carries it too.
func TestWithheldDecisionCarriesThePolicy(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
		doc := provenanceDoc(3)
		doc.ApprovalRequiredClasses = []ChangeClass{ChangeIndex}
		return doc, nil
	})
	d := gate.Authorize(context.Background(), validIndexRequest())
	if d.Verdict != VerdictQueueApproval || d.Reason != ReasonApprovalRequired {
		t.Fatalf("decision = %s, want queue_approval/approval_required", d.LedgerLabel())
	}
	assertProvenance(t, p.recorded[0], 3, provenanceSHA)
}

func TestHardStopCarriesNoPolicy(t *testing.T) {
	p := &provenanceGate{}
	runtime := provenanceRuntime()
	runtime.EmergencyStop = true
	gate := p.gate(runtime, func(ActionRequest) (Document, error) {
		return provenanceDoc(9), nil
	})
	d := gate.Authorize(context.Background(), validIndexRequest())
	if d.Verdict != VerdictBlocked || d.Reason != ReasonEmergencyStop {
		t.Fatalf("decision = %s, want blocked/emergency_stop", d.LedgerLabel())
	}
	assertProvenance(t, p.recorded[0], 0, "")
	if got := p.reads.Load(); got != 0 {
		t.Fatalf("a hard stop read the policy %d times, want 0", got)
	}
}

func TestPolicyReadFailureCarriesNoPolicy(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
		return Document{Version: 4, SHA256: provenanceSHA}, errors.New("connection refused")
	})
	d := gate.Authorize(context.Background(), validIndexRequest())
	if d.Verdict != VerdictBlocked || d.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(d.Detail, "connection refused") {
		t.Fatalf("decision = %s %q, want blocked/policy_unavailable naming the error",
			d.LedgerLabel(), d.Detail)
	}
	if len(p.recorded) != 1 {
		t.Fatalf("recorded %d decisions, want the blocked one", len(p.recorded))
	}
	assertProvenance(t, p.recorded[0], 0, "")
}

func TestOperatorAndRevertDecisionsCarryThePolicy(t *testing.T) {
	operator := validIndexRequest()
	operator.OperatorApproved = true
	cases := map[string]ActionRequest{"operator": operator,
		"revert": revertRequest(false), "operator revert": revertRequest(true)}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p := &provenanceGate{}
			gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
				return provenanceDoc(5), nil
			})
			d := gate.Authorize(context.Background(), req)
			if d.Verdict != VerdictExecute {
				t.Fatalf("decision = %s (%s), want execute", d.LedgerLabel(), d.Detail)
			}
			assertProvenance(t, p.recorded[0], 5, provenanceSHA)
		})
	}
}

// policy-05: a revert runs when the policy is unreadable. It evaluated no
// document, so it names none.
func TestRevertUnderUnreadablePolicyCarriesNoPolicy(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
		return Document{}, ErrNotFound
	})
	d := gate.Authorize(context.Background(), revertRequest(false))
	if d.Verdict != VerdictExecute || d.PolicyReadError == "" {
		t.Fatalf("decision = %s %+v, want an execute naming the read error",
			d.LedgerLabel(), d)
	}
	assertProvenance(t, p.recorded[0], 0, "")
}

// Explain evaluates the same way and records nothing; it names the policy.
func TestExplainNamesThePolicyAndRecordsNothing(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(ActionRequest) (Document, error) {
		return provenanceDoc(2), nil
	})
	d := gate.(Explainer).Explain(context.Background(), validIndexRequest())
	assertProvenance(t, d, 2, provenanceSHA)
	if len(p.recorded) != 0 {
		t.Fatalf("Explain recorded %d decisions, want 0", len(p.recorded))
	}
}

// Concurrent decisions never see each other's policy: each request reads
// its own version and must carry exactly that one.
func TestConcurrentDecisionsCarryTheirOwnPolicy(t *testing.T) {
	p := &provenanceGate{}
	gate := p.gate(provenanceRuntime(), func(req ActionRequest) (Document, error) {
		var version int64
		if _, err := fmt.Sscanf(req.TargetObjs[0], "public.t%d", &version); err != nil {
			return Document{}, err
		}
		doc := provenanceDoc(version)
		doc.SHA256 = fmt.Sprintf("%064x", version)
		return doc, nil
	})
	const racers = 32
	results := make([]Decision, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := validIndexRequest()
			req.TargetObjs = []string{fmt.Sprintf("public.t%d", i+1)}
			results[i] = gate.(Explainer).Explain(context.Background(), req)
		}(i)
	}
	wg.Wait()
	for i, d := range results {
		want := int64(i + 1)
		assertProvenance(t, d, want, fmt.Sprintf("%064x", want))
	}
}

func TestDocumentFromPolicyStampsVersionProfileAndHash(t *testing.T) {
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal staffed profile: %v", err)
	}
	doc, err := DocumentFromPolicy(Policy{Version: 12, Profile: "staffed", Document: raw})
	if err != nil {
		t.Fatalf("DocumentFromPolicy: %v", err)
	}
	sum := sha256.Sum256(raw)
	if doc.Version != 12 || doc.Profile != ProfileStaffed ||
		doc.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("document = v%d %q %q, want v12 staffed %x", doc.Version, doc.Profile,
			doc.SHA256, sum)
	}
	if len(doc.AllowedChangeClasses) == 0 {
		t.Fatal("document lost its allowed change classes")
	}
}

func TestDocumentFromPolicyRejectsInvalidDocuments(t *testing.T) {
	cases := map[string][]byte{"empty": nil, "not json": []byte("{"),
		"unknown field": []byte(`{"allowed_change_classes":["index"],"bogus":1}`)}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := DocumentFromPolicy(Policy{Version: 3, Profile: "staffed",
				Document: raw})
			if err == nil {
				t.Fatalf("document %q parsed; want an error", raw)
			}
			if doc.Version != 0 || doc.SHA256 != "" {
				t.Fatalf("an unparsable document carries v%d %q, want none", doc.Version,
					doc.SHA256)
			}
		})
	}
}

// The zero document carries no provenance: a gate whose reader stamps
// nothing records version 0 (Explain path, nil reader).
func TestNilPolicyReaderCarriesNoPolicy(t *testing.T) {
	gate := NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return provenanceRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
	})
	d := gate.(Explainer).Explain(context.Background(), validIndexRequest())
	if d.Reason != ReasonPolicyUnavailable {
		t.Fatalf("decision = %s, want policy_unavailable", d.LedgerLabel())
	}
	assertProvenance(t, d, 0, "")
}
