package earned

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// One autonomy scale per class (ux-06b, decision D6). The operator's
// trust level, execution mode and tier3 switches, the emergency stop, the
// replica role and the class's earned level all bound what pg_sage may do
// unattended. ModeOf folds them into one of four sentences, in the gate's
// own order and from the gate's own inputs, so the sentence never claims
// more autonomy than the gate grants. The standing policy document
// (allowed classes, approval classes, windows, budgets) can only lower it.

// ModeName is one step of the scale.
type ModeName string

// The modes, least autonomy first.
const (
	ModeObserves      ModeName = "observes"
	ModeAsksFirst     ModeName = "asks_first"
	ModeActsThenTells ModeName = "acts_then_tells"
	ModeActsInWindow  ModeName = "acts_in_window"
)

// Sentence is the mode as the page says it; an unknown mode watches only.
func (m ModeName) Sentence() string {
	switch m {
	case ModeAsksFirst:
		return "asks first"
	case ModeActsThenTells:
		return "acts, then tells you"
	case ModeActsInWindow:
		return "acts in the window"
	}
	return "watches only"
}

// Mode is a class's step with why: the first item is the input that
// limits it (or, for an acting mode, the bound it acts within).
type Mode struct {
	Name    ModeName `json:"mode"`
	Because []string `json:"because"`
}

// ClassContract is what the gate reads of a class's action contracts:
// the most restrictive tier, rollback class and approval rule among them.
type ClassContract struct {
	RiskTier         policy.RiskTier
	Rollback         policy.RollbackClass
	ApprovalRequired bool
	ChangeClass      policy.ChangeClass
}

// Ceiling is one database's gate inputs (the executor's runtime state).
// The zero value has no executor and watches only.
type Ceiling struct {
	GateAttached          bool
	ExecutorEnabled       bool
	EmergencyStop         bool
	Replica               bool
	TrustLevel            string
	ExecutionMode         string
	Tier3Safe             bool
	Tier3Moderate         bool
	WindowConfigured      bool
	SQLValidationDegraded bool
	LedgerEnforced        bool
	RampStart             time.Time
	SafeRampAge           time.Duration
	ModerateRampAge       time.Duration
	Now                   time.Time
	Contracts             map[ActionClass]ClassContract
	// ClassTrust is a policy set's per-class trust ceiling (fleetpol-01a);
	// nil adds no cap. It can only lower the trust level.
	ClassTrust map[policy.ChangeClass]string
}

// AnnotateMode fills every row's mode from ceil; call it after
// Limiter.AnnotateTrust so rows carry their effective level.
func AnnotateMode(v *TrustView, ceil Ceiling) {
	if v == nil {
		return
	}
	for i := range v.Rows {
		m := ModeOf(v.Rows[i], ceil)
		v.Rows[i].Mode, v.Rows[i].ModeBecause = m.Name, m.Because
	}
}

// verdict is the gate's verdict before the ledger restricts it.
type verdict int

const (
	verdictObserve verdict = iota
	verdictAsk
	verdictExecute
)

type baseVerdict struct {
	verdict verdict
	because []string
}

// ModeOf is row's mode under ceil, in the gate's order: hard stops, the
// operator's settings, the class's tier and switches, then the ledger.
func ModeOf(row TrustRow, ceil Ceiling) Mode {
	if why := hardStop(ceil); why != "" {
		return observes(why)
	}
	contract, ok := ceil.Contracts[row.Class]
	if !ok {
		return observes(fmt.Sprintf("no action contract for class %s", row.Class))
	}
	trust, note := ceil.trustFor(contract.ChangeClass)
	if why := settingsStop(trust, ceil.ExecutionMode, note); why != "" {
		return observes(why)
	}
	base := tierVerdict(row, contract, trust, ceil)
	if base.verdict == verdictExecute {
		base = boundVerdict(contract, ceil, base)
	}
	return ledgerMode(row, contract, ceil, base)
}

func observes(because ...string) Mode {
	return Mode{Name: ModeObserves, Because: because}
}

func hardStop(ceil Ceiling) string {
	switch {
	case !ceil.GateAttached:
		return "no executor policy is attached to this database"
	case !ceil.ExecutorEnabled:
		return "executor is disabled"
	case ceil.EmergencyStop:
		return "emergency stop is on"
	case ceil.Replica:
		return "database is a replica: changes run on the primary"
	}
	return ""
}

// trustRank orders the trust levels; an unknown level ranks below all.
func trustRank(level string) int {
	switch level {
	case policy.TrustObservation:
		return 0
	case policy.TrustAdvisory:
		return 1
	case policy.TrustAutonomous:
		return 2
	}
	return -1
}

// trustFor is the trust level that binds a change class: the operator's,
// lowered by a policy set's ceiling on the class.
func (c Ceiling) trustFor(class policy.ChangeClass) (string, string) {
	set, ok := c.ClassTrust[class]
	if !ok || trustRank(set) >= trustRank(c.TrustLevel) {
		return c.TrustLevel, ""
	}
	return set, fmt.Sprintf("policy set trust ceiling %s on %s", set, class)
}

// settingsStop is the operator setting that makes the gate observe only,
// in its order; an unknown mode fails closed here (the gate's config
// validation refuses one).
func settingsStop(trust, mode, note string) string {
	switch {
	case trust == policy.TrustObservation && note != "":
		return note
	case trust == policy.TrustObservation:
		return "trust ceiling observation"
	case mode == policy.ExecutionManual:
		return "execution mode is manual"
	case trustRank(trust) < 0 && note != "":
		return note
	case trustRank(trust) < 0:
		return fmt.Sprintf("unknown trust level %q", trust)
	case mode != policy.ExecutionAuto && mode != policy.ExecutionApproval:
		return fmt.Sprintf("unknown execution mode %q", mode)
	}
	return ""
}

func ask(why string) baseVerdict     { return baseVerdict{verdictAsk, []string{why}} }
func observe(why string) baseVerdict { return baseVerdict{verdictObserve, []string{why}} }

// tierVerdict is the gate's document approval rule, then its tier
// decision (tierDecision): execution mode, tier, tier3 switch and ramp.
func tierVerdict(row TrustRow, c ClassContract, trust string, ceil Ceiling) baseVerdict {
	if c.ApprovalRequired {
		return ask(fmt.Sprintf("%s always needs approval", row.Class))
	}
	if ceil.ExecutionMode == policy.ExecutionApproval {
		return ask("execution mode is approval")
	}
	ledgerDecides := ceil.LedgerEnforced && row.Kind == KindSelfInitiated
	switch c.RiskTier {
	case policy.RiskReadOnly:
		return baseVerdict{verdictExecute, []string{"read-only"}}
	case policy.RiskSafe:
		if !ceil.Tier3Safe {
			return observe("tier3_safe is off")
		}
		return rampVerdict(ceil, c, policy.SpecSafeRampAge, ceil.SafeRampAge, ledgerDecides,
			"tier3_safe is on")
	case policy.RiskModerate:
		if trust == policy.TrustAdvisory {
			return ask(advisoryReason(trust, ceil.TrustLevel))
		}
		if !ceil.Tier3Moderate {
			return observe("tier3_moderate is off")
		}
		return rampVerdict(ceil, c, policy.SpecModerateRampAge, ceil.ModerateRampAge,
			ledgerDecides, "tier3_moderate is on")
	case policy.RiskHigh:
		return ask("high-risk changes always ask first")
	}
	return observe(fmt.Sprintf("unknown risk tier %q", c.RiskTier))
}

func advisoryReason(trust, operator string) string {
	if trust != operator {
		return "policy set trust ceiling advisory"
	}
	return "trust ceiling advisory"
}

// rampVerdict executes once the trust ramp has elapsed; a class the
// ledger decides is never held by the clock (the gate's rampSatisfied).
func rampVerdict(ceil Ceiling, c ClassContract, spec, configured time.Duration,
	ledgerDecides bool, on string) baseVerdict {
	if ledgerDecides {
		return baseVerdict{verdictExecute, []string{on}}
	}
	need := policy.RampAge(configured, spec, c.Rollback)
	now := ceil.Now
	if now.IsZero() {
		now = time.Now()
	}
	if ceil.RampStart.IsZero() || now.Sub(ceil.RampStart) < need {
		return observe(fmt.Sprintf("trust ramp: %s changes wait %s after start",
			c.RiskTier, need))
	}
	return baseVerdict{verdictExecute, []string{on}}
}

// boundVerdict applies what can only restrict an execute verdict: SQL
// validation without cgo, then the configured maintenance window.
func boundVerdict(c ClassContract, ceil Ceiling, base baseVerdict) baseVerdict {
	if ceil.SQLValidationDegraded && c.RiskTier != policy.RiskReadOnly {
		return ask("SQL validation is degraded (built without cgo): unattended changes " +
			"need approval")
	}
	if c.RiskTier == policy.RiskModerate && !ceil.WindowConfigured {
		return observe("no maintenance window is configured (trust.maintenance_window)")
	}
	return base
}
