package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// GET /api/v1/agents/freezes goes through lateKillSwitch (agentgov-07):
// before startAgentKill builds the switch it answers ErrUnavailable (503),
// and afterwards it is the switch's own answer, which without a control
// database also fails closed.
func TestLateKillSwitch_OpenFreezes(t *testing.T) {
	prev := agentKill.Load()
	t.Cleanup(func() { agentKill.Store(prev) })

	agentKill.Store(nil)
	list, err := lateKillSwitch{}.OpenFreezes(context.Background())
	if !errors.Is(err, agentguard.ErrUnavailable) || len(list.Items) != 0 {
		t.Fatalf("before startup: %+v, %v; want ErrUnavailable", list, err)
	}

	sw, err := agentguard.NewSwitch(agentguard.KillDeps{
		Config: agentguard.DefaultKillConfig(),
		Targets: func(context.Context) ([]agentguard.KillTarget, error) {
			return nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	agentKill.Store(sw)
	_, err = lateKillSwitch{}.OpenFreezes(context.Background())
	if !errors.Is(err, agentguard.ErrUnavailable) {
		t.Fatalf("a switch without a control database: %v, want ErrUnavailable", err)
	}
}
