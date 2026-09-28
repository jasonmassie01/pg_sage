package executor

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// D1 verdict-diff guard. Every typed action contract is evaluated under both
// built-in profiles for an autonomous, an advisory and an operator-approved
// request. baselineVerdicts were captured from HEAD before the refusal set
// was enforced (54c8688); both profiles produced identical verdicts there.
// Enforcing the refusal set may change a verdict only where
// intendedVerdictChanges lists it. For today's catalog that list is empty.

type matrixRow struct {
	scenario, actionType, verdict, reason string
}

var baselineVerdicts = []matrixRow{
	{"autonomous", "analyze_table", "execute", "authorized"},
	{"autonomous", "alter_system_guc", "execute", "authorized"},
	{"autonomous", "alter_database_guc", "execute", "authorized"},
	{"autonomous", "diagnose_lock_blockers", "blocked", "change_class_not_allowed"},
	{"autonomous", "diagnose_runaway_query", "blocked", "change_class_not_allowed"},
	{"autonomous", "diagnose_connection_exhaustion", "blocked", "change_class_not_allowed"},
	{"autonomous", "diagnose_wal_replication", "blocked", "change_class_not_allowed"},
	{"autonomous", "diagnose_standby_conflicts", "execute", "authorized"},
	{"autonomous", "prepare_sequence_capacity_migration", "blocked",
		"change_class_not_allowed"},
	{"autonomous", "cancel_backend", "queue_approval", "approval_required"},
	{"autonomous", "terminate_backend", "queue_approval", "approval_required"},
	{"autonomous", "vacuum_table", "execute", "authorized"},
	{"autonomous", "diagnose_freeze_blockers", "blocked", "change_class_not_allowed"},
	{"autonomous", "set_table_autovacuum", "execute", "authorized"},
	{"autonomous", "diagnose_vacuum_pressure", "blocked", "change_class_not_allowed"},
	{"autonomous", "plan_bloat_remediation", "blocked", "change_class_not_allowed"},
	{"autonomous", "reindex_concurrently", "queue_approval", "approval_required"},
	{"autonomous", "prepare_query_rewrite", "blocked", "change_class_not_allowed"},
	{"autonomous", "promote_role_work_mem", "blocked", "change_class_not_allowed"},
	{"autonomous", "retire_query_hint", "execute", "authorized"},
	{"autonomous", "apply_query_hint", "queue_approval", "approval_required"},
	{"autonomous", "investigate_query_plan", "blocked", "change_class_not_allowed"},
	{"autonomous", "create_statistics", "blocked", "change_class_not_allowed"},
	{"autonomous", "prepare_parameterized_query", "blocked", "change_class_not_allowed"},
	{"autonomous", "retention_delete", "execute", "authorized"},
	{"autonomous", "revert_created_index", "execute", "authorized"},
	{"autonomous", "create_index_concurrently", "execute", "authorized"},
	{"autonomous", "drop_unused_index", "execute", "authorized"},
	{"autonomous", "alter_table", "queue_approval", "approval_required"},
	{"autonomous", "ddl_preflight", "blocked", "change_class_not_allowed"},
	{"advisory", "analyze_table", "execute", "authorized"},
	{"advisory", "alter_system_guc", "queue_approval", "approval_required"},
	{"advisory", "alter_database_guc", "queue_approval", "approval_required"},
	{"advisory", "diagnose_lock_blockers", "blocked", "change_class_not_allowed"},
	{"advisory", "diagnose_runaway_query", "blocked", "change_class_not_allowed"},
	{"advisory", "diagnose_connection_exhaustion", "blocked", "change_class_not_allowed"},
	{"advisory", "diagnose_wal_replication", "blocked", "change_class_not_allowed"},
	{"advisory", "diagnose_standby_conflicts", "execute", "authorized"},
	{"advisory", "prepare_sequence_capacity_migration", "blocked", "change_class_not_allowed"},
	{"advisory", "cancel_backend", "queue_approval", "approval_required"},
	{"advisory", "terminate_backend", "queue_approval", "approval_required"},
	{"advisory", "vacuum_table", "execute", "authorized"},
	{"advisory", "diagnose_freeze_blockers", "blocked", "change_class_not_allowed"},
	{"advisory", "set_table_autovacuum", "queue_approval", "approval_required"},
	{"advisory", "diagnose_vacuum_pressure", "blocked", "change_class_not_allowed"},
	{"advisory", "plan_bloat_remediation", "blocked", "change_class_not_allowed"},
	{"advisory", "reindex_concurrently", "queue_approval", "approval_required"},
	{"advisory", "prepare_query_rewrite", "blocked", "change_class_not_allowed"},
	{"advisory", "promote_role_work_mem", "blocked", "change_class_not_allowed"},
	{"advisory", "retire_query_hint", "execute", "authorized"},
	{"advisory", "apply_query_hint", "queue_approval", "approval_required"},
	{"advisory", "investigate_query_plan", "blocked", "change_class_not_allowed"},
	{"advisory", "create_statistics", "blocked", "change_class_not_allowed"},
	{"advisory", "prepare_parameterized_query", "blocked", "change_class_not_allowed"},
	{"advisory", "retention_delete", "queue_approval", "approval_required"},
	{"advisory", "revert_created_index", "queue_approval", "approval_required"},
	{"advisory", "create_index_concurrently", "queue_approval", "approval_required"},
	{"advisory", "drop_unused_index", "queue_approval", "approval_required"},
	{"advisory", "alter_table", "queue_approval", "approval_required"},
	{"advisory", "ddl_preflight", "blocked", "change_class_not_allowed"},
	{"operator", "analyze_table", "execute", "operator_approved"},
	{"operator", "alter_system_guc", "execute", "operator_approved"},
	{"operator", "alter_database_guc", "execute", "operator_approved"},
	{"operator", "diagnose_lock_blockers", "blocked", "change_class_not_allowed"},
	{"operator", "diagnose_runaway_query", "blocked", "change_class_not_allowed"},
	{"operator", "diagnose_connection_exhaustion", "blocked", "change_class_not_allowed"},
	{"operator", "diagnose_wal_replication", "blocked", "change_class_not_allowed"},
	{"operator", "diagnose_standby_conflicts", "blocked", "change_class_not_allowed"},
	{"operator", "prepare_sequence_capacity_migration", "blocked", "change_class_not_allowed"},
	{"operator", "cancel_backend", "execute", "operator_approved"},
	{"operator", "terminate_backend", "execute", "operator_approved"},
	{"operator", "vacuum_table", "execute", "operator_approved"},
	{"operator", "diagnose_freeze_blockers", "blocked", "change_class_not_allowed"},
	{"operator", "set_table_autovacuum", "execute", "operator_approved"},
	{"operator", "diagnose_vacuum_pressure", "blocked", "change_class_not_allowed"},
	{"operator", "plan_bloat_remediation", "blocked", "change_class_not_allowed"},
	{"operator", "reindex_concurrently", "execute", "operator_approved"},
	{"operator", "prepare_query_rewrite", "blocked", "change_class_not_allowed"},
	{"operator", "promote_role_work_mem", "blocked", "change_class_not_allowed"},
	{"operator", "retire_query_hint", "execute", "operator_approved"},
	{"operator", "apply_query_hint", "execute", "operator_approved"},
	{"operator", "investigate_query_plan", "blocked", "change_class_not_allowed"},
	{"operator", "create_statistics", "blocked", "change_class_not_allowed"},
	{"operator", "prepare_parameterized_query", "blocked", "change_class_not_allowed"},
	{"operator", "retention_delete", "execute", "operator_approved"},
	{"operator", "revert_created_index", "execute", "operator_approved"},
	{"operator", "create_index_concurrently", "execute", "operator_approved"},
	{"operator", "drop_unused_index", "execute", "operator_approved"},
	{"operator", "alter_table", "execute", "operator_approved"},
	{"operator", "ddl_preflight", "blocked", "change_class_not_allowed"},
	// Intent-level contracts built outside the executor catalog (MCP
	// candidates and the online-migration runtime).
	{"autonomous", "mcp_create_index", "execute", "authorized"},
	{"advisory", "mcp_create_index", "execute", "authorized"},
	{"operator", "mcp_create_index", "execute", "operator_approved"},
	{"autonomous", "online_migration", "queue_approval", "approval_required"},
	{"advisory", "online_migration", "queue_approval", "approval_required"},
	{"operator", "online_migration", "execute", "operator_approved"},
}

// intendedVerdictChanges lists rows whose verdict the precise refusal
// mapping is meant to change, keyed "scenario/actionType". Empty: no action
// in today's catalog changes verdict under the built-in profiles.
var intendedVerdictChanges = map[string]matrixRow{}

var matrixNow = time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC) // Monday 02:00

func matrixRuntime(scenario string) policy.RuntimeState {
	state := policy.RuntimeState{
		ExecutorEnabled: true, TrustLevel: policy.TrustAutonomous,
		ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
		RampStart:          matrixNow.Add(-40 * 24 * time.Hour),
		InConfiguredWindow: true, WindowConfigured: true,
	}
	if scenario == "advisory" {
		state.TrustLevel = policy.TrustAdvisory
	}
	return state
}

func matrixContract(t *testing.T, actionType string) (*policy.ActionContract, string) {
	t.Helper()
	switch actionType {
	case "mcp_create_index":
		return &policy.ActionContract{
			ActionType: "create_index", RiskTier: policy.RiskSafe,
			RollbackClass: policy.RollbackReversible,
		}, string(policy.ChangeIndex)
	case "online_migration":
		return &policy.ActionContract{
			ActionType: "online_migration", RiskTier: policy.RiskModerate,
			RollbackClass: policy.RollbackForwardFixOnly,
		}, string(policy.ChangeOnlineMigration)
	}
	contract, ok := ContractForActionType(actionType)
	if !ok {
		t.Fatalf("no contract for %q", actionType)
	}
	return policyContract(contract), changeClassForActionType(actionType)
}

func matrixDecision(
	t *testing.T, doc policy.Document, row matrixRow,
) policy.Decision {
	t.Helper()
	runtime := matrixRuntime(row.scenario)
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now: func() time.Time { return matrixNow },
	}).(policy.Explainer)
	contract, feature := matrixContract(t, row.actionType)
	req := policy.ActionRequest{
		Contract: contract, Feature: feature, ExplainFamily: true,
		OperatorApproved: row.scenario == "operator",
		TargetObjs:       []string{"public.orders"},
	}
	if row.actionType == "retention_delete" {
		// AuthorizeRetention's shape: an internal control running under the
		// owner's retention contract.
		req.InternalControl, req.OwnerDeclared = true, true
	}
	return gate.Explain(context.Background(), req)
}

func TestPolicyVerdictMatrixUnchangedByRefusalSet(t *testing.T) {
	profiles := []policy.Document{policy.StaffedProfile(), policy.UnattendedProfile()}
	for _, doc := range profiles {
		for _, row := range baselineVerdicts {
			want := row
			if changed, ok := intendedVerdictChanges[row.scenario+"/"+row.actionType]; ok {
				want = changed
			}
			got := matrixDecision(t, doc, row)
			if string(got.Verdict) != want.verdict || string(got.Reason) != want.reason {
				t.Errorf("%s %s/%s = %s/%s (%s), want %s/%s", doc.Profile,
					row.scenario, row.actionType, got.Verdict, got.Reason, got.Detail,
					want.verdict, want.reason)
			}
		}
	}
}

// With the refusal set emptied the gate must reproduce the same table: the
// refusal set is the only thing D1 lets change a verdict.
func TestPolicyVerdictMatrixWithoutRefusalSetMatchesBaseline(t *testing.T) {
	for _, doc := range []policy.Document{policy.StaffedProfile(), policy.UnattendedProfile()} {
		doc.RefusalSet = nil
		for _, row := range baselineVerdicts {
			got := matrixDecision(t, doc, row)
			if string(got.Verdict) != row.verdict || string(got.Reason) != row.reason {
				t.Errorf("%s %s/%s without refusals = %s/%s, want %s/%s", doc.Profile,
					row.scenario, row.actionType, got.Verdict, got.Reason,
					row.verdict, row.reason)
			}
		}
	}
}

// retention_delete without an owner declaration is not reachable today
// (AuthorizeRetention only runs for a declared retention contract), but the
// refusal set must send it to a human if it ever is.
func TestPolicyVerdictRetentionWithoutOwnerIsRefused(t *testing.T) {
	contract, feature := matrixContract(t, "retention_delete")
	runtime := matrixRuntime("autonomous")
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
		Now: func() time.Time { return matrixNow },
	})
	got := gate.Authorize(context.Background(), policy.ActionRequest{
		Contract: contract, Feature: feature, InternalControl: true,
		TargetObjs: []string{"public.orders"},
	})
	if got.Verdict != policy.VerdictQueueApproval ||
		got.Reason != policy.ReasonRefusedByPolicy || got.Detail != "unrollbackable" {
		t.Fatalf("retention without owner = %s/%s/%s, want queue_approval/"+
			"refused_by_policy/unrollbackable", got.Verdict, got.Reason, got.Detail)
	}
}

func TestPolicyVerdictMatrixCoversEveryContract(t *testing.T) {
	covered := map[string]int{}
	for _, row := range baselineVerdicts {
		covered[row.actionType]++
	}
	types := contractActionTypes()
	if len(types) < 30 {
		t.Fatalf("parsed %d contract types from source, want >= 30", len(types))
	}
	for _, actionType := range types {
		if covered[actionType] != 3 {
			t.Errorf("%s has %d matrix rows, want 3 (autonomous, advisory, operator)",
				actionType, covered[actionType])
		}
	}
}

// contractActionTypes reads every case of ContractForActionType from source,
// so a new contract cannot be added without a matrix row.
func contractActionTypes() []string {
	source, err := os.ReadFile("action_contract.go")
	if err != nil {
		panic(err)
	}
	body := regexp.MustCompile(`(?s)func ContractForActionType.*?\r?\n}\r?\n`).Find(source)
	matches := regexp.MustCompile(`case "([a-z_]+)":`).FindAllSubmatch(body, -1)
	types := make([]string, 0, len(matches))
	for _, match := range matches {
		types = append(types, string(match[1]))
	}
	return types
}
