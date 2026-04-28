package fleet

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

func TestBuildActionFamilyReadinessAnalyzeSupportedCloudSQL(t *testing.T) {
	cfg := readinessTestConfig("autonomous")
	caps := ProviderCapabilities{
		Provider: "cloud-sql",
	}

	got := BuildActionFamilyReadiness(cfg, caps, "auto", false, time.Now())
	analyze := actionReadiness(t, got, "analyze_table")

	if !analyze.Supported {
		t.Fatalf("analyze supported = false, reason %q",
			analyze.BlockedReason)
	}
	if analyze.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("decision = %q, want execute", analyze.Decision)
	}
}

func TestBuildActionFamilyReadinessBlocksUnsupportedProvider(t *testing.T) {
	cfg := readinessTestConfig("autonomous")
	caps := ProviderCapabilities{Provider: "azure"}

	got := BuildActionFamilyReadiness(cfg, caps, "auto", false, time.Now())
	analyze := actionReadiness(t, got, "analyze_table")

	if analyze.Supported {
		t.Fatal("azure should not be supported by current action contracts")
	}
	if analyze.BlockedReason == "" {
		t.Fatal("expected provider blocked reason")
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
	cfg := readinessTestConfig("autonomous")

	got := BuildProviderCapabilities(
		cfg, "rds", false, "auto", false, time.Now())

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
	cfg := readinessTestConfig("autonomous")
	caps := ProviderCapabilities{Provider: "postgres"}

	got := BuildActionFamilyReadiness(cfg, caps, "auto", false, time.Now())

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
	cfg := readinessTestConfig("autonomous")
	caps := ProviderCapabilities{Provider: "postgres", IsReplica: true}

	got := BuildActionFamilyReadiness(cfg, caps, "auto", false, time.Now())
	analyze := actionReadiness(t, got, "analyze_table")

	if analyze.Supported {
		t.Fatal("replica should block analyze_table execution")
	}
	if analyze.BlockedReason != "target database is a replica" {
		t.Fatalf("blocked reason = %q", analyze.BlockedReason)
	}
}

func TestBuildProviderCapabilitiesPermissionUnknownBlocksAutoSafe(t *testing.T) {
	cfg := readinessTestConfig("autonomous")

	got := BuildProviderCapabilities(
		cfg, "postgres", false, "auto", false, time.Now())

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
