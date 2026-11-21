package earned

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// CHECK-02 (ux-06b): ModeOf must agree with the standing gate's explain
// verdict for the same inputs on every row. The gate runs for real
// (policy.NewGate) with the ceiling as its runtime state, the ledger level
// as its limiter's answer and a permissive document (the unattended
// profile without its refusal set): ModeOf reads only the ceiling and the
// ledger, so the document can only lower the gate's verdict further.

type fixedLimit struct {
	level      Level
	downgraded bool
}

func (l fixedLimit) Limit(context.Context, policy.ActionRequest) (policy.AutonomyLimit, error) {
	return policy.AutonomyLimit{Level: int(l.level), Granted: int(l.level),
		Downgraded: l.downgraded, Class: "test"}, nil
}

func (fixedLimit) Governs(policy.ActionRequest) bool { return true }

func runtimeOf(ceil Ceiling) policy.RuntimeState {
	return policy.RuntimeState{ExecutorEnabled: ceil.ExecutorEnabled,
		EmergencyStop: ceil.EmergencyStop, IsReplica: ceil.Replica,
		TrustLevel: ceil.TrustLevel, ExecutionMode: ceil.ExecutionMode,
		Tier3Safe: ceil.Tier3Safe, Tier3Moderate: ceil.Tier3Moderate,
		RampStart: ceil.RampStart, SafeRampAge: ceil.SafeRampAge,
		ModerateRampAge: ceil.ModerateRampAge, WindowConfigured: ceil.WindowConfigured,
		InConfiguredWindow: ceil.WindowConfigured, TrustWindow: "",
		SQLValidationDegraded: ceil.SQLValidationDegraded}
}

// gateMode asks the real gate to explain row's class under ceil and maps
// its verdict onto the scale.
func gateMode(row TrustRow, ceil Ceiling) ModeName {
	c, ok := ceil.Contracts[row.Class]
	if !ok {
		return ModeObserves // the executor fails an unknown type closed
	}
	doc := policy.UnattendedProfile()
	doc.RefusalSet = nil
	cfg := policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtimeOf(ceil), nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now: func() time.Time { return ceil.Now },
	}
	if ceil.LedgerEnforced {
		cfg.Autonomy = fixedLimit{level: rowLevel(row), downgraded: len(row.Downgrades) > 0}
	}
	contract := &policy.ActionContract{ActionType: "explain_" + string(row.Class),
		RiskTier: c.RiskTier, RollbackClass: c.Rollback}
	if c.ApprovalRequired {
		contract.Guardrails = []policy.Guardrail{policy.GuardrailApprovalRequired}
	}
	req := policy.ActionRequest{Contract: contract, ExplainFamily: true,
		Feature: string(c.ChangeClass), TargetObjs: []string{"public.orders"}}
	if row.Kind == KindIncident {
		req.IncidentFamily = string(row.Family)
	}
	explainer := policy.NewGate(cfg).(policy.Explainer)
	return verdictMode(explainer.Explain(context.Background(), req), c, row)
}

func verdictMode(d policy.Decision, c ClassContract, row TrustRow) ModeName {
	switch d.Verdict {
	case policy.VerdictExecute:
		windowed := c.RiskTier == policy.RiskModerate || c.RiskTier == policy.RiskHigh ||
			(row.Kind == KindIncident && d.Reason == policy.ReasonAutonomyL3)
		if windowed {
			return ModeActsInWindow
		}
		return ModeActsThenTells
	case policy.VerdictQueueApproval:
		return ModeAsksFirst
	}
	return ModeObserves
}

func TestModeOf_HandTableAgreesWithTheGate(t *testing.T) {
	for _, tc := range handModeCases() {
		if got := gateMode(tc.row, tc.ceil); got != tc.want {
			t.Errorf("%s: gate says %s, the hand table %s", tc.name, got, tc.want)
		}
	}
}

// everyGateCeiling crosses everyCeiling with the ramp, the window and
// degraded validation.
func everyGateCeiling() []Ceiling {
	var out []Ceiling
	for _, base := range everyCeiling() {
		for _, ramp := range []time.Duration{60 * 24 * time.Hour, 2 * time.Hour, 30 * time.Minute} {
			for _, window := range []bool{true, false} {
				for _, degraded := range []bool{false, true} {
					c := base
					c.RampStart, c.WindowConfigured = modeEpoch.Add(-ramp), window
					c.SQLValidationDegraded = degraded
					c.SafeRampAge, c.ModerateRampAge = time.Hour, time.Hour
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func everyModeRow() []TrustRow {
	var out []TrustRow
	for class := range modeContracts() {
		for level := L0; level <= L3; level++ {
			for _, kind := range []string{KindSelfInitiated, KindIncident} {
				out = append(out, modeRow(kind, class, level))
			}
		}
		down := selfRow(class, L3)
		l1 := L1
		down.Effective, down.Downgrades = &l1, []Downgrade{{Reason: "failover"}}
		out = append(out, down)
	}
	return out
}

func TestModeOf_AgreesWithTheGateOnEveryRow(t *testing.T) {
	rows, ceilings := everyModeRow(), everyGateCeiling()
	stops := []func(*Ceiling){
		func(*Ceiling) {},
		func(c *Ceiling) { c.EmergencyStop = true },
		func(c *Ceiling) { c.Replica = true },
		func(c *Ceiling) { c.ExecutorEnabled = false },
	}
	checked, disagree := 0, 0
	for _, ceil := range ceilings {
		for i, stop := range stops {
			if i > 0 && ceil.SQLValidationDegraded {
				continue // stops win before validation; one pass is enough
			}
			c := with(ceil, stop)
			for _, row := range rows {
				checked++
				mine, gate := ModeOf(row, c), gateMode(row, c)
				if mine.Name != gate {
					disagree++
					if disagree <= 10 {
						t.Errorf("%s %s at %s under %s: ModeOf %s %v, gate %s", row.Kind,
							row.Class, row.Level, describe(c), mine.Name, mine.Because, gate)
					}
				}
			}
		}
	}
	if disagree != 0 || checked < 10000 {
		t.Fatalf("%d of %d rows disagree with the gate (want 0 of at least 10000)",
			disagree, checked)
	}
	t.Logf("CHECK-02: %d rows, 0 disagreements", checked)
}

func describe(c Ceiling) string {
	return fmt.Sprintf("trust=%s mode=%s safe=%v moderate=%v ledger=%v ramp=%s window=%v "+
		"degraded=%v estop=%v replica=%v enabled=%v", c.TrustLevel, c.ExecutionMode,
		c.Tier3Safe, c.Tier3Moderate, c.LedgerEnforced, c.Now.Sub(c.RampStart),
		c.WindowConfigured, c.SQLValidationDegraded, c.EmergencyStop, c.Replica,
		c.ExecutorEnabled)
}
