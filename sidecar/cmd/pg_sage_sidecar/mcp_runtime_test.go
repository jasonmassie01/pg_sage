package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMCPStandingGateSelectsStandaloneSoleInstance(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	gate := &mcpGateRecorder{decision: policy.Decision{
		Verdict: policy.VerdictPark, EvidenceID: "standalone-evidence",
	}}
	manager.RegisterInstance(mcpTestInstance("standalone", 0, gate))

	decision := (&fleetStandingPolicyGate{manager: manager}).Authorize(
		context.Background(), policy.ActionRequest{},
	)

	require.Equal(t, "standalone-evidence", decision.EvidenceID)
	require.Equal(t, 1, gate.calls)
}

func TestMCPStandingGateSelectsFleetOrMetaInstanceByDatabaseID(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	first := &mcpGateRecorder{decision: policy.Decision{
		Verdict: policy.VerdictPark, EvidenceID: "first-evidence",
	}}
	second := &mcpGateRecorder{decision: policy.Decision{
		Verdict: policy.VerdictPark, EvidenceID: "second-evidence",
	}}
	manager.RegisterInstance(mcpTestInstance("orders", 41, first))
	manager.RegisterInstance(mcpTestInstance("analytics", 42, second))
	databaseID := int64(42)

	decision := (&fleetStandingPolicyGate{manager: manager}).Authorize(
		context.Background(), policy.ActionRequest{DatabaseID: &databaseID},
	)

	require.Equal(t, "second-evidence", decision.EvidenceID)
	require.Zero(t, first.calls)
	require.Equal(t, 1, second.calls)
}

func TestMCPStandingGateFailsClosedForUntargetedMultiInstanceRequest(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	first := &mcpGateRecorder{}
	second := &mcpGateRecorder{}
	manager.RegisterInstance(mcpTestInstance("orders", 41, first))
	manager.RegisterInstance(mcpTestInstance("analytics", 42, second))

	decision := (&fleetStandingPolicyGate{manager: manager}).Authorize(
		context.Background(), policy.ActionRequest{},
	)

	require.Equal(t, policy.VerdictBlocked, decision.Verdict)
	require.Equal(t, policy.ReasonPolicyUnavailable, decision.Reason)
	require.Zero(t, first.calls)
	require.Zero(t, second.calls)
}

func mcpTestInstance(
	name string, databaseID int, gate policy.Gate,
) *fleet.DatabaseInstance {
	instanceExecutor := executor.New(
		nil, config.DefaultConfig(), nil, time.Now(), nil,
	)
	instanceExecutor.WithPolicyGate(gate)
	return &fleet.DatabaseInstance{
		Name: name, DatabaseID: databaseID, Executor: instanceExecutor,
	}
}

type mcpGateRecorder struct {
	decision policy.Decision
	calls    int
}

func (recorder *mcpGateRecorder) Authorize(
	context.Context, policy.ActionRequest,
) policy.Decision {
	recorder.calls++
	return recorder.decision
}
