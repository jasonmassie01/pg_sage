package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D1 T5: the policy contract carries the executor contract's rollback class.
func TestPolicyContractPreservesRollbackClass(t *testing.T) {
	for _, actionType := range contractActionTypes() {
		contract, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("no contract for %q", actionType)
		}
		got := policyContract(contract)
		if string(got.RollbackClass) != contract.RollbackClass {
			t.Errorf("%s: policy RollbackClass = %q, want %q",
				actionType, got.RollbackClass, contract.RollbackClass)
		}
		if err := policy.ValidateContract(*got); err != nil {
			t.Errorf("%s: policy contract invalid: %v", actionType, err)
		}
	}
}

// Index drops are derivable: each carries the index definition as its
// rollback. They are not non_dup_object_drop (earned autonomy, §10.4).
func TestPolicyContractMarksIndexDropsDerivable(t *testing.T) {
	for actionType, want := range map[string]policy.DropKind{
		"drop_unused_index":         policy.DropDerivable,
		"revert_created_index":      policy.DropDerivable,
		"create_index_concurrently": "",
		"alter_table":               "",
		"vacuum_table":              "",
	} {
		contract, _ := ContractForActionType(actionType)
		if got := policyContract(contract).DropKind; got != want {
			t.Errorf("%s DropKind = %q, want %q", actionType, got, want)
		}
	}
}

// set_table_autovacuum ships ALTER TABLE ... RESET as its rollback, so it
// is reversible, not forward-fix-only.
func TestSetTableAutovacuumIsReversible(t *testing.T) {
	contract, _ := ContractForActionType("set_table_autovacuum")
	if contract.RollbackClass != "reversible" {
		t.Fatalf("set_table_autovacuum RollbackClass = %q, want reversible",
			contract.RollbackClass)
	}
}

// Owner authority for an unrollbackable retention delete is D5's explicit
// sage.table_contract.retention_column: the delete must run on the column
// the owner declared. A pre-D5 contract (no declared column) has none.
func TestAuthorizeRetentionDeclaresOwnerAuthority(t *testing.T) {
	tests := []struct {
		name             string
		column, declared string
		window           time.Duration
		want             bool
	}{
		{"declared column", "created_at", "created_at", 30 * 24 * time.Hour, true},
		{"no declared column", "created_at", "", 30 * 24 * time.Hour, false},
		{"declared a different column", "created_at", "ingested_at",
			30 * 24 * time.Hour, false},
		{"missing column", "", "", 30 * 24 * time.Hour, false},
		{"zero window", "created_at", "created_at", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := &custodianGateCapture{verdict: policy.Decision{
				Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate,
			}}
			exec := New(nil, &config.Config{}, zeroTime(), nopLog)
			exec.WithPolicyGate(gate)

			_, err := exec.ExecuteRetention(context.Background(), RetentionRequest{
				Target: "public.events", Column: tt.column, DeclaredColumn: tt.declared,
				Window: tt.window, Cutoff: time.Now(), BatchLimit: 100,
			})

			// Authorized: without a database the delete cannot run, so the
			// executor stops at admission, after the gate saw the request.
			if errors.Is(err, ErrActionWithheld) ||
				!strings.Contains(fmt.Sprint(err), "database pool unavailable") {
				t.Fatalf("ExecuteRetention = %v, want authorized then not admitted", err)
			}
			if gate.request.OwnerDeclared != tt.want {
				t.Fatalf("OwnerDeclared = %v, want %v", gate.request.OwnerDeclared, tt.want)
			}
			if gate.request.Contract.RollbackClass != policy.RollbackNotReversible {
				t.Fatalf("retention RollbackClass = %q, want not_reversible",
					gate.request.Contract.RollbackClass)
			}
		})
	}
}

// Through the real standing gate with the built-in unattended profile and
// a satisfied moderate ramp: a declared column is authorized as before; a
// contract without one is sent to a human (refused_by_policy).
func TestAuthorizeRetentionThroughStandingGate(t *testing.T) {
	now := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	exec := New(nil, wave1PolicyConfig("autonomous"),
		now.Add(-40*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), func() time.Time { return now })
	request := RetentionRequest{
		Target: "public.events", Column: "created_at", Window: 30 * 24 * time.Hour,
		Cutoff: now.Add(-30 * 24 * time.Hour), BatchLimit: 100, Candidates: 10,
	}

	_, undeclared := exec.ExecuteRetention(context.Background(), request)
	request.DeclaredColumn = "created_at"
	_, declared := exec.ExecuteRetention(context.Background(), request)

	if undeclared == nil || !strings.Contains(undeclared.Error(), "refused_by_policy") {
		t.Fatalf("undeclared column = %v, want withheld refused_by_policy", undeclared)
	}
	// Authorized: without a database the delete stops at admission.
	if errors.Is(declared, ErrActionWithheld) ||
		!strings.Contains(fmt.Sprint(declared), "database pool unavailable") {
		t.Fatalf("declared column = %v, want authorized", declared)
	}
}

func TestStandingPolicyDecisionCarriesLockCeiling(t *testing.T) {
	got := standingPolicyDecision(policy.Decision{
		Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized,
		LockCeilingMS: 3000,
	})
	if got.Decision != PolicyDecisionExecute || got.LockCeilingMS != 3000 {
		t.Fatalf("decision = %s ceiling=%d, want execute ceiling=3000",
			got.Decision, got.LockCeilingMS)
	}
}

func TestRefusedDecisionIsQueuedWithToken(t *testing.T) {
	got := standingPolicyDecision(policy.Decision{
		Verdict: policy.VerdictQueueApproval, Reason: policy.ReasonRefusedByPolicy,
		Detail: policy.RefusalUnrollbackable,
	})
	if got.Decision != PolicyDecisionQueueApproval || !got.RequiresApproval ||
		got.BlockedReason != "refused_by_policy" || got.Detail != "unrollbackable" {
		t.Fatalf("refused decision = %+v, want queued refused_by_policy/unrollbackable", got)
	}
}

// D1 T6: in-transaction DDL takes min(policy ceiling, safety); CONCURRENTLY
// and top-level statements keep the safety value because CIC/RIC wait on
// older transactions and a short cap would mostly leave INVALID indexes.
func TestDDLLockTimeoutMS(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		safety  int
		ceiling int64
		want    int
	}{
		{"in-tx capped", "ALTER TABLE public.t SET (fillfactor = 90)", 30000, 3000, 3000},
		{"in-tx ceiling equals safety", "ALTER TABLE public.t SET (fillfactor = 90)",
			3000, 3000, 3000},
		{"in-tx safety tighter", "ALTER TABLE public.t SET (fillfactor = 90)",
			1000, 3000, 1000},
		{"in-tx no ceiling", "ALTER TABLE public.t SET (fillfactor = 90)", 30000, 0, 30000},
		{"create index concurrently", "CREATE INDEX CONCURRENTLY i ON public.t (a)",
			30000, 3000, 30000},
		{"drop index concurrently", "DROP INDEX CONCURRENTLY public.i", 30000, 3000, 30000},
		{"reindex concurrently", "REINDEX INDEX CONCURRENTLY public.i", 30000, 3000, 30000},
		{"vacuum top-level", "VACUUM public.t", 30000, 3000, 30000},
		{"alter system top-level", "ALTER SYSTEM SET work_mem = '8MB'", 30000, 3000, 30000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ddlLockTimeoutMS(tt.sql, tt.safety, tt.ceiling); got != tt.want {
				t.Fatalf("ddlLockTimeoutMS = %d, want %d", got, tt.want)
			}
		})
	}
}
