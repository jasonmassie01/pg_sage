package executor

import (
	"testing"
	"time"
)

// migration-05 (CHECK-25): the cancel guidance for a DDL that holds a
// lock queue is a HIGH-risk cancel_backend candidate. HIGH risk always
// needs approval: the standing gate never executes it, even with every
// autonomy switch on and an always-open window.
func TestMigrationQueueCancel_HighRiskNeverAutoExecutes(t *testing.T) {
	contract, ok := ContractForActionType("cancel_backend")
	if !ok {
		t.Fatal("cancel_backend has no typed contract")
	}
	contract.BaseRiskTier = "high" // the migration lock-queue candidate's tier
	for _, mode := range []string{"auto", "approval", "manual"} {
		cfg := windowedAutonomousConfig()
		cfg.Trust.Tier3HighRisk = true
		d := policyVerdict(contract, verdictInput{cfg: cfg, mode: mode,
			now: time.Now(), rampStart: time.Now().Add(-365 * 24 * time.Hour)})
		if d.Decision == PolicyDecisionExecute {
			t.Fatalf("mode %s: HIGH cancel_backend decision = execute (%+v)", mode, d)
		}
		if d.RiskTier != "high" {
			t.Fatalf("mode %s: decision risk tier = %q, want high", mode, d.RiskTier)
		}
	}
}
