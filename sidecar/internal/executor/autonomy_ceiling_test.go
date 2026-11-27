package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-06b: the Autonomy page's sentence reads the executor's ceiling from
// the same snapshot the gate's runtime state comes from, so a lowered
// trust level shows on the next read without a restart.

func modeCeilingExecutor(t *testing.T) *Executor {
	t.Helper()
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, false
	cfg.Trust.MaintenanceWindow = "0 2 * * *"
	cfg.Trust.RampSafeHours = 12
	e := New(nil, cfg, time.Now().Add(-40*24*time.Hour), func(string, string, ...any) {})
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	return e
}

func TestAutonomyCeiling_ReadsTheGateSnapshot(t *testing.T) {
	e := modeCeilingExecutor(t)
	e.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	e.WithAutonomy(failClosedTestLimiter{})
	c := e.AutonomyCeiling(context.Background(), true)
	if !c.GateAttached || !c.ExecutorEnabled || c.EmergencyStop || !c.Replica ||
		c.TrustLevel != "autonomous" || c.ExecutionMode != "auto" || !c.Tier3Safe ||
		c.Tier3Moderate || !c.WindowConfigured || !c.LedgerEnforced {
		t.Fatalf("ceiling = %+v", c)
	}
	if c.SafeRampAge != 12*time.Hour || c.RampStart.IsZero() || c.Now.IsZero() {
		t.Fatalf("ramp = start %v safe %v now %v", c.RampStart, c.SafeRampAge, c.Now)
	}
	if c.SQLValidationDegraded == ASTValidationAvailable() {
		t.Fatalf("degraded = %v, AST validation available = %v", c.SQLValidationDegraded,
			ASTValidationAvailable())
	}
	if c.ClassTrust != nil {
		t.Fatalf("class trust = %v; the gate enforces no policy-set ceiling yet", c.ClassTrust)
	}
	if _, ok := c.Contracts[earned.ClassIndexCreate]; !ok {
		t.Fatalf("contracts lack index_create: %v", c.Contracts)
	}
}

func TestAutonomyCeiling_LiveChangesShowOnTheNextRead(t *testing.T) {
	e := modeCeilingExecutor(t)
	e.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	ctx := context.Background()
	if got := e.AutonomyCeiling(ctx, false).TrustLevel; got != "autonomous" {
		t.Fatalf("trust = %q, want autonomous", got)
	}
	if err := e.SetTrustLevel("advisory"); err != nil {
		t.Fatal(err)
	}
	e.SetExecutionMode("approval")
	e.WithEmergencyStopCheck(func(context.Context) bool { return true })
	c := e.AutonomyCeiling(ctx, false)
	if c.TrustLevel != "advisory" || c.ExecutionMode != "approval" || !c.EmergencyStop ||
		c.LedgerEnforced {
		t.Fatalf("after live changes = %+v", c)
	}
	e.SetTrustCeiling(&TrustCeiling{Tier3Safe: false, Tier3Moderate: false})
	if c := e.AutonomyCeiling(ctx, false); c.Tier3Safe || c.WindowConfigured {
		t.Fatalf("lowered ceiling not read: %+v", c)
	}
}

func TestAutonomyCeiling_FailsClosedWithoutAGateOrExecutor(t *testing.T) {
	e := modeCeilingExecutor(t)
	if c := e.AutonomyCeiling(context.Background(), false); c.GateAttached {
		t.Fatalf("no standing gate, yet attached: %+v", c)
	}
	var nilExec *Executor
	if c := nilExec.AutonomyCeiling(context.Background(), false); c.GateAttached ||
		c.ExecutorEnabled || c.TrustLevel != "" {
		t.Fatalf("nil executor ceiling = %+v, want zero", c)
	}
	noCfg := New(nil, nil, time.Time{}, func(string, string, ...any) {})
	noCfg.WithEmergencyStopCheck(func(context.Context) bool { return false })
	noCfg.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	if c := noCfg.AutonomyCeiling(context.Background(), false); c.TrustLevel != "" {
		t.Fatalf("no config, trust = %q; want empty (unknown, fails closed)", c.TrustLevel)
	}
}

func TestAutonomyClassContracts_AreTheGatesContracts(t *testing.T) {
	got := AutonomyClassContracts()
	want := map[earned.ActionClass]earned.ClassContract{
		earned.ClassVacuum: {RiskTier: policy.RiskSafe,
			Rollback: policy.RollbackNoRollbackNeeded, ChangeClass: policy.ChangeVacuum},
		earned.ClassIndexCreate: {RiskTier: policy.RiskModerate,
			Rollback: policy.RollbackReversible, ChangeClass: policy.ChangeIndex},
		earned.ClassReindex: {RiskTier: policy.RiskModerate,
			Rollback: policy.RollbackReversible, ApprovalRequired: true,
			ChangeClass: policy.ChangeIndex},
		earned.ClassRetention: {RiskTier: policy.RiskModerate,
			Rollback: policy.RollbackNotReversible, ChangeClass: policy.ChangeRetention},
		earned.ClassBackendCancel: {RiskTier: policy.RiskModerate,
			Rollback: "mitigation_only", ApprovalRequired: true,
			ChangeClass: policy.ChangeBackendSignal},
		earned.ClassConfigGUC: {RiskTier: policy.RiskModerate,
			Rollback: policy.RollbackReversible, ApprovalRequired: true,
			ChangeClass: policy.ChangeConfigGUC},
		earned.ClassBackendTerminate: {RiskTier: policy.RiskHigh,
			Rollback: policy.RollbackNotReversible, ApprovalRequired: true,
			ChangeClass: policy.ChangeBackendSignal},
	}
	for class, contract := range want {
		if got[class] != contract {
			t.Errorf("%s = %+v, want %+v", class, got[class], contract)
		}
	}
	for _, class := range []earned.ActionClass{earned.ClassSlotDrop, earned.ClassUnclassified} {
		if c, ok := got[class]; ok {
			t.Errorf("%s has no action type, yet a contract %+v", class, c)
		}
	}
	got[earned.ClassVacuum] = earned.ClassContract{}
	if AutonomyClassContracts()[earned.ClassVacuum].RiskTier != policy.RiskSafe {
		t.Fatal("a caller's edit reached the shared contracts")
	}
}

type failClosedTestLimiter struct{}

func (failClosedTestLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	return policy.AutonomyLimit{}, nil
}
