package earned

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

const (
	windowBound   = "MODERATE changes act only inside the maintenance window"
	incidentBound = "L3 incident remediations act only inside the maintenance window"
	notEnforced   = "earned levels are not enforced (sre.autonomy.enforce is off)"
)

// ledgerMode applies the earned level as the gate's restrictAutonomy
// does: it only restricts. Below L2 pg_sage writes a script at most; L2,
// or a verdict that already asks, hands the change to a human; L3 acts.
func ledgerMode(row TrustRow, c ClassContract, ceil Ceiling, base baseVerdict) Mode {
	if base.verdict == verdictObserve {
		return Mode{Name: ModeObserves, Because: base.because}
	}
	if !ceil.LedgerEnforced || c.RiskTier == policy.RiskReadOnly {
		return unledgeredMode(c, base, ceil.LedgerEnforced)
	}
	level, capped := gateLevel(row, c.Rollback)
	why := levelReason(row, level, capped)
	switch {
	case level < L2:
		return observes(why)
	case base.verdict == verdictAsk:
		return Mode{Name: ModeAsksFirst, Because: append(base.because, why)}
	case level == L2:
		return Mode{Name: ModeAsksFirst, Because: []string{why}}
	case row.Kind == KindIncident:
		return Mode{Name: ModeActsInWindow, Because: []string{incidentBound, why}}
	}
	return actingMode(c, append([]string{why}, base.because...))
}

// unledgeredMode is the verdict when the ledger does not judge the class.
func unledgeredMode(c ClassContract, base baseVerdict, enforced bool) Mode {
	m := Mode{Name: ModeAsksFirst, Because: base.because}
	if base.verdict != verdictAsk {
		m = actingMode(c, base.because)
	}
	if !enforced {
		m.Because = append([]string{notEnforced}, m.Because...)
	}
	return m
}

// actingMode is an execute verdict: a MODERATE change waits for the
// window, a SAFE one acts and notifies.
func actingMode(c ClassContract, because []string) Mode {
	if c.RiskTier == policy.RiskModerate || c.RiskTier == policy.RiskHigh {
		return Mode{Name: ModeActsInWindow, Because: append([]string{windowBound}, because...)}
	}
	return Mode{Name: ModeActsThenTells, Because: because}
}

// rowLevel is the level the gate allows the row now: its effective level,
// else (no limiter annotated it) its level capped by its class cap.
func rowLevel(row TrustRow) Level {
	level := MinLevel(row.Level, row.Cap)
	if row.Effective != nil {
		level = *row.Effective
	}
	return min(max(level, L0), L3)
}

// gateLevel caps rowLevel by the contract's reversibility, as the gate's
// effectiveAutonomyLevel does; capped reports that the cap bound.
func gateLevel(row TrustRow, rollback policy.RollbackClass) (Level, bool) {
	level, limit := rowLevel(row), RollbackCap(rollback)
	if limit < level {
		return limit, true
	}
	return level, false
}

func levelReason(row TrustRow, level Level, capped bool) string {
	switch {
	case capped && level <= L1:
		return fmt.Sprintf("irreversible class: earned level capped at %s", level)
	case capped:
		return fmt.Sprintf("mitigation-only class: earned level capped at %s", level)
	case len(row.Downgrades) > 0 && level < row.Level:
		reasons := make([]string, 0, len(row.Downgrades))
		for _, d := range row.Downgrades {
			reasons = append(reasons, d.Reason)
		}
		return "downgraded: " + strings.Join(reasons, ",")
	}
	return "earned level " + level.String()
}
