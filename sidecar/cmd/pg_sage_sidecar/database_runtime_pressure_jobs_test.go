package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// guard-01b wiring: the enforcer ticks only when this process may propose
// (fleet leader, writable primary), a failed tick is logged and retried,
// and a pressure cancel honours the operator's protected backends. No
// concurrent access tests: the loop runs one tick at a time.

type countingTicker struct {
	ticks int
	res   pressure.TickResult
	err   error
}

func (c *countingTicker) Tick(context.Context) (pressure.TickResult, error) {
	c.ticks++
	return c.res, c.err
}

func TestPressureJobTick_RunsOnlyWhenAllowed(t *testing.T) {
	c := &countingTicker{res: pressure.TickResult{Checked: 1, Proposed: 1}}
	runPressureTick(context.Background(), "orders", c, func() bool { return false })
	if c.ticks != 0 {
		t.Fatalf("a follower or replica ticked %d times", c.ticks)
	}
	runPressureTick(context.Background(), "orders", c, func() bool { return true })
	if c.ticks != 1 {
		t.Fatalf("the leader ticked %d times, want 1", c.ticks)
	}
	c.err = errors.New("relation sage.pressure_jobs does not exist")
	runPressureTick(context.Background(), "orders", c, func() bool { return true })
	if c.ticks != 2 {
		t.Fatalf("a failing tick stopped the loop (%d ticks)", c.ticks)
	}
}

func TestPressureJobProtectedBackends_CopyTheSREActionSettings(t *testing.T) {
	if roles, apps := protectedBackends(nil); roles != nil || apps != nil {
		t.Fatalf("no config = %v %v, want none", roles, apps)
	}
	cfg := config.DefaultConfig()
	cfg.SRE.Actions.ProtectedRoles = []string{"replicator"}
	cfg.SRE.Actions.ProtectedApplications = []string{"billing-api"}
	roles, apps := protectedBackends(cfg)
	if len(roles) != 1 || roles[0] != "replicator" || len(apps) != 1 || apps[0] != "billing-api" {
		t.Fatalf("protected = %v %v", roles, apps)
	}
	roles[0] = "changed"
	if cfg.SRE.Actions.ProtectedRoles[0] != "replicator" {
		t.Fatal("protectedBackends shares the config's slice")
	}
}

// Without a pool there is no checker and no job store: the routes answer
// unavailable and no runner is registered.
func TestPressureJobStartPressure_NoPoolBuildsNothing(t *testing.T) {
	rt := &databaseRuntime{}
	rt.startPressure()
	if rt.pressure != nil || rt.pressureJobs != nil || len(rt.features) != 0 {
		t.Fatalf("no pool built %v %v %v", rt.pressure, rt.pressureJobs, rt.features)
	}
}
