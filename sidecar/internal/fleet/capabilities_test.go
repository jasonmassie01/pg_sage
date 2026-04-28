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
