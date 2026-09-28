package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// gateExecutor is an executor with a real standing gate over an in-memory
// unattended policy whose windows are always open.
func gateExecutor(t *testing.T, trust string) *executor.Executor {
	t.Helper()
	e := executor.New(nil, readinessTestConfig(trust),
		time.Now().Add(-90*24*time.Hour), func(string, string, ...any) {})
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	e.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	e.EnableStandingPolicyDocument(doc, nil)
	return e
}

func TestBuildActionFamilyReadinessAnalyzeSupportedCloudSQL(t *testing.T) {
	caps := ProviderCapabilities{Provider: "cloud-sql"}
	explain := ExecutorFamilyExplainer(gateExecutor(t, "autonomous"))

	analyze := actionReadiness(t, buildActionFamilyReadiness(caps, explain), "analyze_table")

	if !analyze.Supported || analyze.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("analyze readiness = %#v, want supported execute", analyze)
	}
}

// Adapter-unsupported and unimplemented families never reach the gate.
func TestBuildActionFamilyReadinessAsksGateOnlyForSupportedFamilies(t *testing.T) {
	var asked []string
	explain := func(contracts []executor.ActionContract, _ bool) []executor.ActionPolicyDecision {
		out := make([]executor.ActionPolicyDecision, len(contracts))
		for i, c := range contracts {
			asked = append(asked, c.ActionType)
			out[i] = executor.ActionPolicyDecision{Decision: executor.PolicyDecisionExecute}
		}
		return out
	}
	got := buildActionFamilyReadiness(ProviderCapabilities{Provider: "postgres"}, explain)
	for _, actionType := range asked {
		if actionType == "create_statistics" || actionType == "promote_role_work_mem" {
			t.Fatalf("unimplemented family %s was sent to the gate", actionType)
		}
	}
	stats := actionReadiness(t, got, "create_statistics")
	if stats.Supported || stats.BlockedReason != "direct execution is not implemented" {
		t.Fatalf("create_statistics = %#v", stats)
	}
	if len(asked) == 0 || len(asked) >= len(got) {
		t.Fatalf("gate asked about %d of %d families", len(asked), len(got))
	}
}

func TestBuildActionFamilyReadinessFailsClosedWithoutExecutor(t *testing.T) {
	got := buildActionFamilyReadiness(ProviderCapabilities{Provider: "postgres"},
		ExecutorFamilyExplainer(nil))
	analyze := actionReadiness(t, got, "analyze_table")
	if analyze.Supported || analyze.BlockedReason != "standing policy unavailable" {
		t.Fatalf("analyze without executor = %#v", analyze)
	}
}

func TestProviderAdapterAddsManagedProviderLimitations(t *testing.T) {
	adapter := AdapterForProvider("cloud sql")

	if adapter.Provider != "cloud-sql" {
		t.Fatalf("provider = %q", adapter.Provider)
	}
	if adapter.Extensions["pg_hint_plan"] != "provider_parameter_required" {
		t.Fatalf("pg_hint_plan status = %q", adapter.Extensions["pg_hint_plan"])
	}
	if adapter.LogAccess != "provider_logging" {
		t.Fatalf("LogAccess = %q", adapter.LogAccess)
	}
	if !adapter.SupportsAction("diagnose_wal_replication") {
		t.Fatal("cloud-sql should support read-only replication diagnostics")
	}
}

func TestBuildProviderCapabilitiesUsesProviderAdapter(t *testing.T) {
	got := BuildProviderCapabilities("rds", false,
		ExecutorFamilyExplainer(gateExecutor(t, "autonomous")))

	if got.Extensions["pg_hint_plan"] != "parameter_group_required" {
		t.Fatalf("pg_hint_plan = %q", got.Extensions["pg_hint_plan"])
	}
	if got.LogAccess != "cloudwatch" {
		t.Fatalf("LogAccess = %q", got.LogAccess)
	}
	if len(got.Limitations) == 0 {
		t.Fatal("expected provider limitations")
	}
}

func TestBuildActionFamilyReadinessIncludesNewAutonomyFamilies(t *testing.T) {
	got := buildActionFamilyReadiness(ProviderCapabilities{Provider: "postgres"},
		ExecutorFamilyExplainer(gateExecutor(t, "autonomous")))

	for _, actionType := range []string{
		"vacuum_table",
		"diagnose_lock_blockers",
		"diagnose_runaway_query",
		"diagnose_connection_exhaustion",
		"diagnose_wal_replication",
		"diagnose_standby_conflicts",
		"prepare_sequence_capacity_migration",
		"cancel_backend",
		"terminate_backend",
		"diagnose_freeze_blockers",
		"diagnose_vacuum_pressure",
		"set_table_autovacuum",
		"plan_bloat_remediation",
		"reindex_concurrently",
		"prepare_query_rewrite",
		"promote_role_work_mem",
		"create_statistics",
		"prepare_parameterized_query",
		"retire_query_hint",
		"ddl_preflight",
	} {
		if actionReadiness(t, got, actionType).ActionType == "" {
			t.Fatalf("%s missing from readiness", actionType)
		}
	}
}

func TestBuildActionFamilyReadinessBlocksReplicaWriteAction(t *testing.T) {
	caps := ProviderCapabilities{Provider: "postgres", IsReplica: true}
	got := buildActionFamilyReadiness(caps,
		ExecutorFamilyExplainer(gateExecutor(t, "autonomous")))
	analyze := actionReadiness(t, got, "analyze_table")

	if analyze.Supported || analyze.BlockedReason != "replica_mutation" {
		t.Fatalf("replica analyze readiness = %#v", analyze)
	}
}

func TestEnsureCapabilitiesBlocksDisabledExecutor(t *testing.T) {
	exec := gateExecutor(t, "autonomous")
	exec.SetExecutorEnabled(false)
	snap := EnsureCapabilities(&DatabaseInstance{Executor: exec}, &InstanceStatus{
		Platform:     "postgres",
		Capabilities: ProviderCapabilities{Provider: "postgres"},
	})

	analyze := actionReadiness(t, snap.Capabilities.ActionFamilies, "analyze_table")
	if analyze.Supported || analyze.BlockedReason != "executor_disabled" {
		t.Fatalf("analyze readiness = %#v", analyze)
	}
}

func TestEnsureCapabilitiesBlocksStoppedInstance(t *testing.T) {
	snap := EnsureCapabilities(
		&DatabaseInstance{Executor: gateExecutor(t, "autonomous"), Stopped: true},
		&InstanceStatus{Platform: "postgres",
			Capabilities: ProviderCapabilities{Provider: "postgres"}})
	analyze := actionReadiness(t, snap.Capabilities.ActionFamilies, "analyze_table")
	if analyze.Supported || analyze.BlockedReason != "emergency stop is active" {
		t.Fatalf("stopped instance readiness = %#v", analyze)
	}
}

func TestEnsureCapabilitiesPreservesCollectedRuntimeEvidence(t *testing.T) {
	inst := &DatabaseInstance{Executor: gateExecutor(t, "autonomous")}
	snap := EnsureCapabilities(inst, &InstanceStatus{
		Platform: "postgres",
		Capabilities: ProviderCapabilities{
			Provider: "postgres",
			Permissions: map[string]CapabilityStatus{
				"analyze": {Status: "ok", Reason: "runtime probe"},
			},
			Extensions: map[string]string{
				"pg_stat_statements": "available",
				"custom_extension":   "available",
			},
			Limitations: []string{"runtime limitation"},
		},
	})

	if snap.Capabilities.Permissions["analyze"].Reason != "runtime probe" ||
		snap.Capabilities.Extensions["custom_extension"] != "available" ||
		len(snap.Capabilities.Limitations) != 1 {
		t.Fatalf("runtime evidence was replaced: %#v", snap.Capabilities)
	}
}

func TestBuildProviderCapabilitiesPermissionUnknownBlocksAutoSafe(t *testing.T) {
	got := BuildProviderCapabilities("postgres", false,
		ExecutorFamilyExplainer(gateExecutor(t, "autonomous")))

	if got.ReadyForAutoSafe {
		t.Fatal("unknown ANALYZE permission should block auto-safe readiness")
	}
	if len(got.Blockers) == 0 {
		t.Fatal("expected readiness blockers")
	}
}

func TestSummarizeReadinessCountsReadyBlockedUnknown(t *testing.T) {
	dbs := []DatabaseStatus{
		{
			Name: "ready",
			Status: &InstanceStatus{Capabilities: ProviderCapabilities{
				Provider:         "postgres",
				ReadyForAutoSafe: true,
			}},
		},
		{
			Name: "blocked",
			Status: &InstanceStatus{Capabilities: ProviderCapabilities{
				Provider: "postgres",
				Blockers: []string{"target is a replica"},
			}},
		},
		{
			Name: "unknown",
			Status: &InstanceStatus{Capabilities: ProviderCapabilities{
				Provider: "unknown",
			}},
		},
	}

	got := SummarizeReadiness(dbs)

	if got.TotalDatabases != 3 || got.ReadyForAutoSafe != 1 ||
		got.Blocked != 1 || got.Unknown != 1 {
		t.Fatalf("summary = %+v", got)
	}
}

func readinessTestConfig(trust string) *config.Config {
	return &config.Config{
		Mode:             "fleet",
		CloudEnvironment: "postgres",
		Trust: config.TrustConfig{
			Level:         trust,
			Tier3Safe:     true,
			Tier3Moderate: false,
			RampStart:     time.Now().Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		},
		Tuner: config.TunerConfig{
			MaxConcurrentAnalyze: 3,
		},
	}
}

func actionReadiness(
	t *testing.T,
	items []ActionFamilyReadiness,
	actionType string,
) ActionFamilyReadiness {
	t.Helper()
	for _, item := range items {
		if item.ActionType == actionType {
			return item
		}
	}
	t.Fatalf("action %q not found in %+v", actionType, items)
	return ActionFamilyReadiness{}
}
