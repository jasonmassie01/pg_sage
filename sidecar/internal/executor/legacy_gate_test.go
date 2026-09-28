package executor

import (
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// ShouldExecute keeps historical trust-ramp assertions meaningful: it asks
// the standing gate (the only policy authority) about a representative
// typed contract for the finding's risk tier.
func ShouldExecute(
	f analyzer.Finding, cfg *config.Config, rampStart time.Time,
	isReplica, emergencyStop bool,
) bool {
	risk := f.ActionRisk
	if risk == "high_risk" {
		risk = "high"
	}
	contract := riskContract(risk)
	if contract.ActionType == "" {
		return false
	}
	decision := policyVerdict(contract, verdictInput{
		cfg: cfg, now: time.Now(), rampStart: rampStart,
		isReplica: isReplica, stopped: emergencyStop,
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
