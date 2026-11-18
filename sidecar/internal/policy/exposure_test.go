package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fleetpol-02b: the policy document's exposure mode. shadow (the default)
// never lets an exposure remediation run or be proposed; enforce hands it
// to the usual tier, trust and approval rules. enable_row_level_security
// always needs approval; the rollback of pg_sage's own exposure change is
// never held by the mode nor refused as a grant expansion or RLS change.

func exposureRequest(actionType, sql string) ActionRequest {
	tier, guardrails := RiskModerate, []Guardrail(nil)
	if actionType == ActionEnableRowLevelSecurity {
		tier, guardrails = RiskHigh, []Guardrail{GuardrailApprovalRequired}
	}
	return ActionRequest{
		Contract: &ActionContract{ActionType: actionType, RiskTier: tier,
			Guardrails: guardrails, RollbackClass: RollbackReversible},
		SQL: sql, Feature: string(ChangeExposure),
		TargetObjs: []string{"public.leads"},
	}
}

func revokeRequest() ActionRequest {
	return exposureRequest(ActionRevokePublicGrant,
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`)
}

func rlsRequest() ActionRequest {
	return exposureRequest(ActionEnableRowLevelSecurity,
		`ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`)
}

func exposureDoc(mode string) Document {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.ExposureMode = mode
	return doc
}

// exposureGate is a gate over doc and runtime whose SQL validation accepts
// everything (the executor's validator is tested in its own package).
func exposureGate(doc Document, runtime RuntimeState, policyErr error) Gate {
	now := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return runtime, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return doc, policyErr
		},
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			return LimitUsage{}, nil
		},
		RecordDecision: func(context.Context, ActionRequest, Decision) (string, error) {
			return "decision-1", nil
		},
		Now: func() time.Time { return now },
	})
}

func TestExposureModeParsesValidatesAndMarshals(t *testing.T) {
	for _, mode := range []string{"", ExposureModeShadow, ExposureModeEnforce} {
		doc := exposureDoc(mode)
		raw, err := MarshalDocument(doc)
		if err != nil {
			t.Fatalf("marshal mode %q: %v", mode, err)
		}
		hasKey := strings.Contains(string(raw), `"exposure":{`)
		if hasKey != (mode != "") {
			t.Fatalf("mode %q: exposure key written=%v in %s", mode, hasKey, raw)
		}
		back, err := ParseDocument(raw)
		if err != nil {
			t.Fatalf("parse mode %q: %v", mode, err)
		}
		if back.ExposureMode != mode {
			t.Fatalf("round trip of %q = %q", mode, back.ExposureMode)
		}
		if got := back.ExposureEnforced(); got != (mode == ExposureModeEnforce) {
			t.Fatalf("ExposureEnforced(%q) = %v", mode, got)
		}
	}
}

// Invalid input: an unknown mode is refused at validation and at parse.
func TestExposureModeRefusesUnknownValues(t *testing.T) {
	for _, mode := range []string{"Enforce", "auto", " shadow", "off"} {
		doc := exposureDoc(mode)
		err := ValidateDocument(doc)
		if err == nil || !strings.Contains(err.Error(), "exposure.mode") {
			t.Fatalf("ValidateDocument(mode %q) = %v, want an exposure.mode error", mode, err)
		}
	}
	raw, err := MarshalDocument(exposureDoc(ExposureModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), `"mode":"enforce"`, `"mode":"later"`, 1)
	if _, err := ParseDocument([]byte(bad)); err == nil {
		t.Fatalf("ParseDocument accepted mode later: %s", bad)
	}
	unknownKey := strings.Replace(string(raw), `"mode":"enforce"`,
		`"mode":"enforce","extra":1`, 1)
	if _, err := ParseDocument([]byte(unknownKey)); err == nil {
		t.Fatal("ParseDocument accepted an unknown key under exposure")
	}
}

// Nil/empty: a stored document without the key reads as shadow.
func TestExposureModeAbsentIsShadow(t *testing.T) {
	raw, err := MarshalDocument(exposureDoc(""))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	delete(wire, "exposure")
	plain, _ := json.Marshal(wire)
	doc, err := ParseDocument(plain)
	if err != nil {
		t.Fatalf("parse a document without exposure: %v", err)
	}
	if doc.ExposureEnforced() || doc.ExposureMode != "" {
		t.Fatalf("absent key = %q enforced=%v, want shadow", doc.ExposureMode,
			doc.ExposureEnforced())
	}
}

// State transition: shadow -> enforce widens; enforce -> shadow does not.
func TestExposureEnforceWidens(t *testing.T) {
	shadow, enforce := exposureDoc(""), exposureDoc(ExposureModeEnforce)
	if !Widens(shadow, enforce) || !Widens(exposureDoc(ExposureModeShadow), enforce) {
		t.Fatal("shadow -> enforce must count as widening")
	}
	if Widens(enforce, shadow) || Widens(enforce, exposureDoc(ExposureModeShadow)) {
		t.Fatal("enforce -> shadow narrows; it must not count as widening")
	}
	if Widens(enforce, enforce) {
		t.Fatal("an unchanged enforce document widens nothing")
	}
}

// Boundary: shadow mode never proposes nor runs, at any trust level.
func TestExposureShadowIsObserveOnly(t *testing.T) {
	for _, mode := range []string{"", ExposureModeShadow} {
		for _, req := range []ActionRequest{revokeRequest(), rlsRequest()} {
			gate := exposureGate(exposureDoc(mode), newTestGateRuntime(), nil)
			got := gate.Authorize(context.Background(), req)
			assertDecision(t, got, VerdictObserveOnly, ReasonExposureShadow)
			approved := req
			approved.OperatorApproved = true
			got = gate.Authorize(context.Background(), approved)
			assertDecision(t, got, VerdictObserveOnly, ReasonExposureShadow)
		}
	}
}

// Error propagation: an unreadable policy blocks (never runs) the action.
func TestExposureUnreadablePolicyBlocks(t *testing.T) {
	gate := exposureGate(Document{}, newTestGateRuntime(), errors.New("store down"))
	got := gate.Authorize(context.Background(), revokeRequest())
	if got.Verdict == VerdictExecute || got.Reason != ReasonPolicyUnavailable {
		t.Fatalf("decision = %s/%s, want blocked policy_unavailable", got.Verdict, got.Reason)
	}
}

// Happy path: enforce plus a granted moderate tier runs the revoke.
func TestExposureEnforceRunsRevokeAtGrantedModerate(t *testing.T) {
	gate := exposureGate(exposureDoc(ExposureModeEnforce), newTestGateRuntime(), nil)
	got := gate.Authorize(context.Background(), revokeRequest())
	assertDecision(t, got, VerdictExecute, ReasonAuthorized)
	if got.RiskTier != RiskModerate {
		t.Fatalf("risk tier = %s, want moderate", got.RiskTier)
	}
}

// Boundary: enforce at a trust level that grants moderate only with
// approval (advisory) proposes instead of running.
func TestExposureEnforceBelowModerateQueuesApproval(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.TrustLevel = TrustAdvisory
	gate := exposureGate(exposureDoc(ExposureModeEnforce), runtime, nil)
	got := gate.Authorize(context.Background(), revokeRequest())
	if got.Verdict != VerdictQueueApproval {
		t.Fatalf("decision = %s/%s, want queue_approval", got.Verdict, got.Reason)
	}
}

// enable_row_level_security is never executed without approval, even at
// the highest trust level with every tier granted (pinned).
func TestExposureEnableRLSAlwaysNeedsApproval(t *testing.T) {
	runtime := newTestGateRuntime() // autonomous, every tier flag granted
	gate := exposureGate(exposureDoc(ExposureModeEnforce), runtime, nil)
	got := gate.Authorize(context.Background(), rlsRequest())
	assertDecision(t, got, VerdictQueueApproval, ReasonApprovalRequired)
	approved := rlsRequest()
	approved.OperatorApproved = true
	got = gate.Authorize(context.Background(), approved)
	if got.Verdict != VerdictExecute {
		t.Fatalf("approved RLS = %s/%s, want execute", got.Verdict, got.Reason)
	}
}

// A rollback of pg_sage's own exposure change runs in shadow mode too, and
// the default refusal set (grant_expansion, rls_change) does not hold it.
func TestExposureRollbackRunsInShadowAndIsNotRefused(t *testing.T) {
	cases := map[string]ActionRequest{
		"regrant": exposureRequest(ActionRevokePublicGrant,
			`GRANT SELECT ON TABLE public.leads TO PUBLIC`),
		"disable": exposureRequest(ActionEnableRowLevelSecurity,
			`ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY`),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			req.Rollback = true
			if req.Contract.ActionType == ActionEnableRowLevelSecurity {
				req.Contract.Guardrails = nil // a rollback carries no approval guardrail
			}
			doc := exposureDoc("")
			if token, refused := RefusedBy(doc, req); refused {
				t.Fatalf("rollback refused by %s", token)
			}
			got := exposureGate(doc, newTestGateRuntime(), nil).
				Authorize(context.Background(), req)
			if got.Verdict != VerdictExecute {
				t.Fatalf("rollback = %s/%s, want execute", got.Verdict, got.Reason)
			}
		})
	}
}

// Invalid input: a re-grant or RLS disable that is not a rollback is
// refused even in enforce mode, and a forward GRANT keeps being a grant
// expansion for any other contract.
func TestExposureInverseForwardIsBlocked(t *testing.T) {
	for _, sql := range []string{`GRANT SELECT ON TABLE public.leads TO PUBLIC`,
		`ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY`} {
		req := revokeRequest()
		req.SQL = sql
		req.OperatorApproved = true
		got := exposureGate(exposureDoc(ExposureModeEnforce), newTestGateRuntime(), nil).
			Authorize(context.Background(), req)
		if got.Verdict != VerdictBlocked || got.Reason != ReasonRefusedByPolicy {
			t.Fatalf("%s = %s/%s, want blocked refused_by_policy", sql, got.Verdict,
				got.Reason)
		}
	}
	other := ActionRequest{Contract: &ActionContract{ActionType: "alter_table",
		RiskTier: RiskModerate}, SQL: `GRANT SELECT ON TABLE public.leads TO PUBLIC`,
		Rollback: true}
	if _, refused := RefusedBy(exposureDoc(""), other); !refused {
		t.Fatal("a GRANT under another contract must stay a grant expansion")
	}
}

// Non-exposure contracts are unaffected by the mode.
func TestExposureModeLeavesOtherContractsAlone(t *testing.T) {
	req := ActionRequest{Contract: &ActionContract{ActionType: "analyze_table",
		RiskTier: RiskSafe, RollbackClass: RollbackNoRollbackNeeded},
		SQL: "ANALYZE public.leads", Feature: string(ChangeAnalyze),
		TargetObjs: []string{"public.leads"}}
	got := exposureGate(exposureDoc(""), newTestGateRuntime(), nil).
		Authorize(context.Background(), req)
	assertDecision(t, got, VerdictExecute, ReasonAuthorized)
	if IsExposureRemediation(req) || IsExposureRemediation(ActionRequest{}) {
		t.Fatal("analyze_table and a request without contract are not exposure actions")
	}
}

// Concurrent access: the gate reads the document per decision; parallel
// decisions in shadow mode are all observe-only.
func TestExposureShadowConcurrentDecisions(t *testing.T) {
	gate := exposureGate(exposureDoc(""), newTestGateRuntime(), nil)
	var wg sync.WaitGroup
	verdicts := make([]Verdict, 16)
	for i := range verdicts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			verdicts[i] = gate.Authorize(context.Background(), revokeRequest()).Verdict
		}(i)
	}
	wg.Wait()
	for i, v := range verdicts {
		if v != VerdictObserveOnly {
			t.Fatalf("decision %d = %s, want observe_only", i, v)
		}
	}
}

func TestExposureReasonIsALedgerReason(t *testing.T) {
	if !ledgerReasonSet[ReasonExposureShadow] {
		t.Fatalf("%s is not in ledgerReasons", ReasonExposureShadow)
	}
	if !knownChangeClasses()[ChangeExposure] {
		t.Fatalf("%s is not a known change class", ChangeExposure)
	}
}
