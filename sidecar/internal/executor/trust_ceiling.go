package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/config"
)

// TrustCeiling is the operator's live outer bound on unattended autonomy:
// the tier3 switches, the configured maintenance window and the rollback
// and cascade bounds. SetTrustCeiling replaces it on a running executor so
// lowering a ceiling takes autonomy back on the next authorization
// (config-01); the trust ledger still gates every promotion beneath it.
type TrustCeiling struct {
	Tier3Safe             bool
	Tier3Moderate         bool
	MaintenanceWindow     string
	RollbackThresholdPct  int
	RollbackWindowMinutes int
	RollbackCooldownDays  int
	CascadeCooldownCycles int
}

// TrustCeilingFromConfig reads the ceiling fields of a trust section.
func TrustCeilingFromConfig(trust config.TrustConfig) TrustCeiling {
	return TrustCeiling{
		Tier3Safe: trust.Tier3Safe, Tier3Moderate: trust.Tier3Moderate,
		MaintenanceWindow:     trust.MaintenanceWindow,
		RollbackThresholdPct:  trust.RollbackThresholdPct,
		RollbackWindowMinutes: trust.RollbackWindowMinutes,
		RollbackCooldownDays:  trust.RollbackCooldownDays,
		CascadeCooldownCycles: trust.CascadeCooldownCycles,
	}
}

// ApplyTo writes the ceiling onto a trust section, leaving the trust level,
// ramp and tier3_high_risk untouched.
func (c TrustCeiling) ApplyTo(trust *config.TrustConfig) {
	trust.Tier3Safe, trust.Tier3Moderate = c.Tier3Safe, c.Tier3Moderate
	trust.MaintenanceWindow = c.MaintenanceWindow
	trust.RollbackThresholdPct = c.RollbackThresholdPct
	trust.RollbackWindowMinutes = c.RollbackWindowMinutes
	trust.RollbackCooldownDays = c.RollbackCooldownDays
	trust.CascadeCooldownCycles = c.CascadeCooldownCycles
}

// StricterTrustCeiling is what a failed ceiling change leaves behind: each
// tier3 switch is on only when both ceilings have it on, and every other
// bound keeps the old value. A failure therefore never raises autonomy.
func StricterTrustCeiling(old, next TrustCeiling) TrustCeiling {
	result := old
	result.Tier3Safe = old.Tier3Safe && next.Tier3Safe
	result.Tier3Moderate = old.Tier3Moderate && next.Tier3Moderate
	return result
}

// SetTrustCeiling replaces this executor's live ceiling; nil clears it so
// the executor follows its config again. The argument is copied.
func (e *Executor) SetTrustCeiling(ceiling *TrustCeiling) {
	var owned *TrustCeiling
	if ceiling != nil {
		copied := *ceiling
		owned = &copied
	}
	e.policyMu.Lock()
	e.trustCeiling = owned
	e.policyMu.Unlock()
}

// TrustCeiling returns the ceiling the next authorization uses. Without a
// config there is no ceiling and the zero value grants nothing.
func (e *Executor) TrustCeiling() TrustCeiling {
	cfg, _, _ := e.policySnapshot()
	if cfg == nil {
		return TrustCeiling{}
	}
	return TrustCeilingFromConfig(cfg.Trust)
}

// rollbackCooldownDays is the live anti-flap cooldown after a rollback.
func (e *Executor) rollbackCooldownDays() int {
	cfg, _, _ := e.policySnapshot()
	if cfg == nil {
		return config.DefaultRollbackCooldownDays
	}
	return cfg.Trust.RollbackCooldownDays
}

// recentlyRolledBack reports a finding still inside the live rollback
// cooldown, so the next cycle does not re-apply what was just reverted.
func (e *Executor) recentlyRolledBack(ctx context.Context, findingID int64) bool {
	return CheckHysteresis(ctx, e.pool, findingID, e.rollbackCooldownDays())
}
