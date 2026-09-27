package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// withTestStandingGate installs a real standing gate over the executor's
// own runtime state (trust, mode, tier flags, ramp, emergency stop), as
// production does, with an in-memory unattended policy whose windows are
// always open. Tests that drive RunCycle need a gate: without one the
// executor fails closed (G4-I01).
func withTestStandingGate(e *Executor) *Executor {
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	e.WithPolicyGate(policy.NewGate(policy.GateConfig{
		Runtime: func(ctx context.Context, req policy.ActionRequest) (policy.RuntimeState, error) {
			return e.standingRuntimeState(ctx, req), nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
	}))
	return e
}
