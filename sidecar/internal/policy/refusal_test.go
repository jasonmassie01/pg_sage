package policy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// D1: refusal_set tokens are typed predicates over the contract (rollback
// class, dropped-object kind) and the request's authority (owner
// declaration, operator approval). A refused self-initiated action is sent
// to a human with reason refused_by_policy; it is never silently dropped.

var refusalNow = time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)

func refusalRuntime() RuntimeState {
	return RuntimeState{
		ExecutorEnabled: true, TrustLevel: TrustAutonomous, ExecutionMode: ExecutionAuto,
		Tier3Safe: true, Tier3Moderate: true, InConfiguredWindow: true,
		WindowConfigured: true, RampStart: refusalNow.Add(-60 * 24 * time.Hour),
	}
}

// refusalGate accepts any SQL so the refusal predicates see statements the
// executor allowlist would reject.
func refusalGate(doc Document, runtime RuntimeState) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return runtime, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return doc, nil
		},
		Now: func() time.Time { return refusalNow },
	})
}

func refusalDoc() Document {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	return doc
}

func moderateContract(rollback RollbackClass, drop DropKind) *ActionContract {
	return &ActionContract{
		ActionType: "synthetic_action", RiskTier: RiskModerate,
		RollbackClass: rollback, DropKind: drop,
	}
}

func refusalRequest(contract *ActionContract, sql string) ActionRequest {
	return ActionRequest{
		Contract: contract, SQL: sql, Feature: string(ChangeIndex),
		TargetObjs: []string{"public.orders"},
	}
}

func authorize(t *testing.T, doc Document, req ActionRequest) Decision {
	t.Helper()
	return refusalGate(doc, refusalRuntime()).Authorize(context.Background(), req)
}

func assertRefused(t *testing.T, got Decision, token string) {
	t.Helper()
	if got.Verdict != VerdictQueueApproval || got.Reason != ReasonRefusedByPolicy {
		t.Fatalf("decision = %s/%s, want queue_approval/refused_by_policy",
			got.Verdict, got.Reason)
	}
	if got.Detail != token {
		t.Fatalf("decision detail = %q, want refusal token %q", got.Detail, token)
	}
}

func assertExecuted(t *testing.T, got Decision, reason Reason) {
	t.Helper()
	if got.Verdict != VerdictExecute || got.Reason != reason {
		t.Fatalf("decision = %s/%s (%s), want execute/%s",
			got.Verdict, got.Reason, got.Detail, reason)
	}
}

func TestRefusalNonDerivableDropQueuesSelfInitiated(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackReversible, DropNonDerivable),
		"SELECT pg_drop_replication_slot('orders_cdc')")

	assertRefused(t, authorize(t, refusalDoc(), req), RefusalNonDupObjectDrop)
}

func TestRefusalOperatorApprovedDropExecutes(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackNotReversible, DropNonDerivable),
		"SELECT pg_drop_replication_slot('orders_cdc')")
	req.OperatorApproved = true

	assertExecuted(t, authorize(t, refusalDoc(), req), ReasonOperatorApproved)
}

// Unused, duplicate and invalid index drops are derivable (the finding
// carries the index definition) and keep their earned autonomy.
func TestRefusalDerivableIndexDropStaysAutonomous(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackReversible, DropDerivable),
		"DROP INDEX CONCURRENTLY public.idx_orders_unused")

	assertExecuted(t, authorize(t, refusalDoc(), req), ReasonAuthorized)
}

func TestRefusalDerivesDropKindFromSQL(t *testing.T) {
	tests := []struct {
		sql     string
		refused bool
	}{
		{"ALTER TABLE public.orders DROP CONSTRAINT orders_fk", true},
		{"ALTER TABLE public.orders DROP COLUMN note", true},
		{"alter table public.orders drop column if exists note", true},
		{"DROP TABLE public.orders", true},
		{"DROP SEQUENCE public.orders_id_seq", true},
		{"DROP SCHEMA archive CASCADE", true},
		{"SELECT pg_drop_replication_slot('orders_cdc')", true},
		{"DROP INDEX CONCURRENTLY public.idx_orders_unused", false},
		{"ALTER TABLE public.orders SET (fillfactor = 90)", false},
		{"CREATE INDEX CONCURRENTLY idx ON public.orders (id)", false},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			req := refusalRequest(moderateContract(RollbackReversible, ""), tt.sql)
			got := authorize(t, refusalDoc(), req)
			if tt.refused {
				assertRefused(t, got, RefusalNonDupObjectDrop)
				return
			}
			assertExecuted(t, got, ReasonAuthorized)
		})
	}
}

func TestRefusalUnrollbackableWithoutOwnerQueues(t *testing.T) {
	for _, class := range []RollbackClass{RollbackNotReversible, RollbackForwardFixOnly} {
		t.Run(string(class), func(t *testing.T) {
			req := refusalRequest(moderateContract(class, ""), "")
			req.Contract.ActionType = "retention_delete"
			req.InternalControl = true

			assertRefused(t, authorize(t, refusalDoc(), req), RefusalUnrollbackable)
		})
	}
}

// retention_delete runs under the owner's retention contract, which is its
// authority; it keeps its autonomy after the moderate ramp.
func TestRefusalOwnerDeclaredRetentionExecutes(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackNotReversible, ""), "")
	req.Contract.ActionType = "retention_delete"
	req.Feature = string(ChangeRetention)
	req.InternalControl = true
	req.OwnerDeclared = true

	assertExecuted(t, authorize(t, refusalDoc(), req), ReasonAuthorized)
}

// "no rollback needed" (VACUUM/ANALYZE) and "not applicable" are not
// unrollbackable: nothing needs undoing.
func TestRefusalRollbackNeutralClassesExecute(t *testing.T) {
	for _, class := range []RollbackClass{
		RollbackReversible, RollbackNoRollbackNeeded, RollbackNotApplicable,
		RollbackApplication, "",
	} {
		t.Run(string(class), func(t *testing.T) {
			req := refusalRequest(moderateContract(class, ""), "")
			req.InternalControl = true
			req.Contract.ActionType = "retention_delete"

			assertExecuted(t, authorize(t, refusalDoc(), req), ReasonAuthorized)
		})
	}
}

func TestRefusalStatementClassTokens(t *testing.T) {
	tests := []struct {
		name  string
		sql   string
		token string
	}{
		{"enable rls", "ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY", RefusalRLSChange},
		{"force rls", "alter table public.orders force row level security", RefusalRLSChange},
		{"no force rls", "ALTER TABLE public.orders NO FORCE ROW LEVEL SECURITY",
			RefusalRLSChange},
		{"create policy", "CREATE POLICY p ON public.orders USING (true)", RefusalRLSChange},
		{"drop policy", "DROP POLICY p ON public.orders", RefusalRLSChange},
		{"grant", "GRANT SELECT ON public.orders TO app", RefusalGrantExpansion},
		{"grant role", "GRANT admin TO app", RefusalGrantExpansion},
		{"alter role", "ALTER ROLE app SUPERUSER", RefusalGrantExpansion},
		{"owner to", "ALTER TABLE public.orders OWNER TO app", RefusalGrantExpansion},
		{"default privileges", "ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO app",
			RefusalGrantExpansion},
		{"extension update", "ALTER EXTENSION pg_stat_statements UPDATE TO '1.11'",
			RefusalMajorUpgrade},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := refusalRequest(moderateContract(RollbackReversible, ""), tt.sql)
			assertRefused(t, authorize(t, refusalDoc(), req), tt.token)
		})
	}
}

func TestRefusalMajorUpgradeActionTypes(t *testing.T) {
	for _, actionType := range []string{"major_upgrade", "extension_update"} {
		req := refusalRequest(moderateContract(RollbackReversible, ""), "")
		req.Contract.ActionType = actionType
		req.InternalControl = true
		req.ExplainFamily = true
		got := refusalGate(refusalDoc(), refusalRuntime()).(Explainer).Explain(
			context.Background(), req)
		assertRefused(t, got, RefusalMajorUpgrade)
	}
}

// A token that is absent from the document does not refuse.
func TestRefusalOnlyListedTokensApply(t *testing.T) {
	doc := refusalDoc()
	doc.RefusalSet = []string{RefusalRLSChange}
	req := refusalRequest(moderateContract(RollbackNotReversible, DropNonDerivable),
		"SELECT pg_drop_replication_slot('orders_cdc')")

	assertExecuted(t, authorize(t, doc, req), ReasonAuthorized)
}

func TestRefusalEmptySetRefusesNothing(t *testing.T) {
	for _, set := range [][]string{nil, {}} {
		doc := refusalDoc()
		doc.RefusalSet = set
		req := refusalRequest(moderateContract(RollbackNotReversible, DropNonDerivable),
			"ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY")

		assertExecuted(t, authorize(t, doc, req), ReasonAuthorized)
	}
}

// Refusal only narrows an execute verdict. A request that would already
// queue or block keeps its verdict and reason.
func TestRefusalNeverWidensVerdict(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackNotReversible, DropNonDerivable),
		"SELECT pg_drop_replication_slot('orders_cdc')")
	advisory := refusalRuntime()
	advisory.TrustLevel = TrustAdvisory
	got := refusalGate(refusalDoc(), advisory).Authorize(context.Background(), req)
	if got.Verdict != VerdictQueueApproval || got.Reason != ReasonApprovalRequired {
		t.Fatalf("advisory decision = %s/%s, want queue_approval/approval_required",
			got.Verdict, got.Reason)
	}

	unramped := refusalRuntime()
	unramped.RampStart = refusalNow.Add(-24 * time.Hour)
	got = refusalGate(refusalDoc(), unramped).Authorize(context.Background(), req)
	if got.Verdict != VerdictBlocked || got.Reason != ReasonTrustRampNotSatisfied {
		t.Fatalf("unramped decision = %s/%s, want blocked/trust_ramp_not_satisfied",
			got.Verdict, got.Reason)
	}
}

func TestRefusalReadOnlyDiagnosticIsNeverRefused(t *testing.T) {
	req := refusalRequest(&ActionContract{
		ActionType: "diagnose_lock_blockers", RiskTier: RiskReadOnly,
		RollbackClass: RollbackNotReversible, DropKind: DropNonDerivable,
	}, "SELECT pg_drop_replication_slot('x')")

	assertExecuted(t, authorize(t, refusalDoc(), req), ReasonAuthorized)
}

func TestRefusalExplainShowsSameReason(t *testing.T) {
	req := refusalRequest(moderateContract(RollbackReversible, DropNonDerivable),
		"SELECT pg_drop_replication_slot('orders_cdc')")
	gate := refusalGate(refusalDoc(), refusalRuntime())

	explained := gate.(Explainer).Explain(context.Background(), req)
	authorized := gate.Authorize(context.Background(), req)

	assertRefused(t, explained, RefusalNonDupObjectDrop)
	assertRefused(t, authorized, RefusalNonDupObjectDrop)
}

func TestRefusedByReportsFirstMatchingTokenInDocumentOrder(t *testing.T) {
	doc := refusalDoc()
	doc.RefusalSet = []string{RefusalUnrollbackable, RefusalNonDupObjectDrop}
	req := refusalRequest(moderateContract(RollbackNotReversible, DropNonDerivable), "")

	token, refused := RefusedBy(doc, req)

	if !refused || token != RefusalUnrollbackable {
		t.Fatalf("RefusedBy = %q/%v, want %q/true", token, refused, RefusalUnrollbackable)
	}
}

func TestRefusedByNilContract(t *testing.T) {
	if token, refused := RefusedBy(refusalDoc(), ActionRequest{}); refused || token != "" {
		t.Fatalf("RefusedBy(nil contract) = %q/%v, want \"\"/false", token, refused)
	}
}

func TestValidateDocumentRejectsUnknownRefusalTokens(t *testing.T) {
	for _, set := range [][]string{
		{"drop_everything"},
		{RefusalRLSChange, "Rls_Change"},
		{""},
		{" rls_change"},
	} {
		doc := refusalDoc()
		doc.RefusalSet = set
		err := ValidateDocument(doc)
		if err == nil {
			t.Fatalf("ValidateDocument(refusal_set=%q) = nil, want error", set)
		}
		if !strings.Contains(err.Error(), "refusal_set") ||
			!strings.Contains(err.Error(), RefusalUnrollbackable) {
			t.Fatalf("error %q must name refusal_set and list the known tokens", err)
		}
	}
}

func TestParseDocumentRejectsUnknownRefusalToken(t *testing.T) {
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	edited := strings.Replace(string(raw), `"rls_change"`, `"drop_everything"`, 1)
	if _, err := ParseDocument([]byte(edited)); err == nil {
		t.Fatal("ParseDocument accepted an unknown refusal token")
	}
}

func TestValidateDocumentAcceptsEveryKnownToken(t *testing.T) {
	doc := refusalDoc()
	doc.RefusalSet = []string{
		RefusalRLSChange, RefusalGrantExpansion, RefusalMajorUpgrade,
		RefusalNonDupObjectDrop, RefusalUnrollbackable,
	}
	if err := ValidateDocument(doc); err != nil {
		t.Fatalf("ValidateDocument(all known tokens) = %v", err)
	}
	doc.RefusalSet = nil
	if err := ValidateDocument(doc); err != nil {
		t.Fatalf("ValidateDocument(empty refusal_set) = %v", err)
	}
}

func TestValidateContractRejectsUnknownRollbackAndDropKind(t *testing.T) {
	base := ActionContract{ActionType: "x", RiskTier: RiskSafe}
	bad := []ActionContract{base, base}
	bad[0].RollbackClass = "sort_of_reversible"
	bad[1].DropKind = "maybe"
	for _, contract := range bad {
		if err := ValidateContract(contract); err == nil {
			t.Fatalf("ValidateContract(%+v) = nil, want error", contract)
		}
	}
	good := base
	good.RollbackClass, good.DropKind = RollbackForwardFixOnly, DropDerivable
	if err := ValidateContract(good); err != nil {
		t.Fatalf("ValidateContract(valid) = %v", err)
	}
}
