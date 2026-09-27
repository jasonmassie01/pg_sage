package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// withTestStandingGate installs a real standing gate over the executor's
// own runtime state (trust, mode, tier flags, ramp, emergency stop), as
// production does, with an in-memory unattended policy whose windows are
// always open. Tests that drive RunCycle need a gate: without one the
// executor fails closed (G4-I01).
func withTestStandingGate(e *Executor) *Executor {
	return withTestStandingGateAt(e, time.Time{})
}

// withTestStandingGateAt pins the gate clock (and the configured-window
// check) to now, for tests that evaluate policy at a fixed time. A zero
// now uses the wall clock.
func withTestStandingGateAt(e *Executor, now time.Time) *Executor {
	if e.pool == nil {
		// No database to read the emergency-stop flag from.
		e.emergencyStopFn = func(context.Context) bool { return false }
	}
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	var clock func() time.Time
	if !now.IsZero() {
		clock = func() time.Time { return now }
	}
	e.EnableStandingPolicyDocument(doc, clock)
	return e
}

// verdictInput describes the runtime a policy verdict is evaluated under.
type verdictInput struct {
	cfg       *config.Config
	mode      string // default "auto"
	now       time.Time
	rampStart time.Time
	isReplica bool
	stopped   bool
	disabled  bool
}

// policyVerdict is the standing gate's verdict for contract under in, via a
// real gate over an executor's runtime (the only policy authority).
func policyVerdict(contract ActionContract, in verdictInput) ActionPolicyDecision {
	e := New(nil, in.cfg, nil, in.rampStart, noopExecLog)
	withTestStandingGateAt(e, in.now)
	stopped := in.stopped
	e.WithEmergencyStopCheck(func(context.Context) bool { return stopped })
	mode := in.mode
	if mode == "" {
		mode = "auto"
	}
	e.SetExecutionMode(mode)
	if in.disabled {
		e.SetExecutorEnabled(false)
	}
	return e.ExplainFamilies(context.Background(), []ActionContract{contract}, in.isReplica)[0]
}

// riskContract returns a real typed contract representative of a risk tier.
func riskContract(risk string) ActionContract {
	actionType := map[string]string{
		"read_only": "diagnose_lock_blockers", "safe": "analyze_table",
		"moderate": "create_index_concurrently", "high": "alter_table",
	}[risk]
	contract, _ := ContractForActionType(actionType)
	return contract
}
