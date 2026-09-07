package executor

import (
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// ShouldExecute keeps historical assertions focused on the canonical policy
// evaluator after the duplicate production gate was removed.
func ShouldExecute(
	f analyzer.Finding, cfg *config.Config, rampStart time.Time,
	isReplica, emergencyStop bool,
) bool {
	risk := f.ActionRisk
	if risk == "high_risk" {
		risk = "high"
	}
	decision := EvaluateActionPolicy(ActionContract{
		ActionType: "legacy_test_action", BaseRiskTier: risk,
	}, ActionPolicyContext{
		Config: cfg, ExecutionMode: "auto", RampStart: rampStart,
		Now: time.Now(), IsReplica: isReplica, EmergencyStop: emergencyStop,
	})
	return decision.Decision == PolicyDecisionExecute
}

func (e *Executor) shouldExecute(
	f analyzer.Finding, isReplica, emergencyStop bool,
) bool {
	e.policyMu.RLock()
	override := e.trustLevelOverride
	e.policyMu.RUnlock()
	cfg := e.cfg
	if override != "" {
		copy := *e.cfg
		copy.Trust.Level = override
		cfg = &copy
	}
	return ShouldExecute(f, cfg, e.rampStart, isReplica, emergencyStop)
}
