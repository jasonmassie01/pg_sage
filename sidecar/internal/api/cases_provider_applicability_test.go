package api

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestEnrichCaseActionPoliciesWithholdsLakebaseAlterSystemWithoutExecutor(t *testing.T) {
	// Provider applicability also matters when observation mode or unavailable
	// execution prevents the standing-policy gate from supplying provider data.
	cfg := caseAutonomousConfig()
	mgr := registerCaseInstance(cfg, nil, &fleet.InstanceStatus{
		Capabilities: fleet.ProviderCapabilities{Provider: "lakebase"},
	})
	c := cases.ProjectFinding(cases.SourceFinding{
		DatabaseName: "prod", Category: "wal_tuning",
		Recommendation: "Enable WAL compression after review.",
		RecommendedSQL: "ALTER SYSTEM SET wal_compression = on;",
	})
	enrichCaseActionPolicies(&c, mgr, "prod")
	a := c.ActionCandidates[0]
	if a.ProposedSQL != "" || a.ScriptOutput != nil || len(a.OutputModes) != 0 || !strings.Contains(c.Why, "Lakebase manages instance settings") {
		t.Fatalf("API retained inapplicable server-setting procedure: %#v", c)
	}
	if a.PolicyDecision == nil || a.PolicyDecision.Decision != "blocked" || a.PolicyDecision.BlockedReason != "provider_managed_setting" {
		t.Fatalf("provider restriction missing: %#v", a.PolicyDecision)
	}
}
