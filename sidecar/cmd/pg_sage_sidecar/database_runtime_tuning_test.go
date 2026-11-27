package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// indexes-12: the deterministic index floor follows tuning.enabled (and
// llm.optimizer.enabled, the switch for index creates), not the LLM: with
// no model it is the only missing-index producer.

func TestIndexFloorOn_FollowsTheSwitchesNotTheLLM(t *testing.T) {
	c := config.DefaultConfig()
	c.LLM.Enabled = false
	if !indexFloorOn(c) {
		t.Fatal("the floor is on by default, LLM or not")
	}
	c.Tuning.Enabled = false
	if indexFloorOn(c) {
		t.Fatal("tuning.enabled=false turns the floor off")
	}
	c = config.DefaultConfig()
	c.LLM.Optimizer.Enabled = false
	if indexFloorOn(c) {
		t.Fatal("llm.optimizer.enabled=false forbids index creates, the floor's too")
	}
	if indexFloorOn(nil) {
		t.Fatal("no configuration, no floor")
	}
}

func TestNewIndexFloor_NilWhenOff(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = config.DefaultConfig()
	cfg.Tuning.Enabled = false
	rt := &databaseRuntime{}
	if f := rt.newIndexFloor(); f != nil {
		t.Fatalf("floor off still built: %v", f)
	}
	cfg = config.DefaultConfig()
	if f := rt.newIndexFloor(); f == nil {
		t.Fatal("floor on but not built")
	}
}

// lab-04a: with clone.provider set, the tuning agent offers the rehearse
// tool and the index floor rehearses; with none, neither does.

func dleCloneConfig(c *config.Config) {
	c.Clone.Provider, c.Clone.DLEEndpoint, c.Clone.DLEToken = "dle",
		"http://127.0.0.1:1", "test-token"
}

func TestBuildCloneRehearser_FollowsTheProvider(t *testing.T) {
	c := config.DefaultConfig()
	if r, err := buildCloneRehearser(c, "", nil, nil, nil); r != nil || err != nil {
		t.Fatalf("clone.provider none: %v, %v", r, err)
	}
	if r, err := buildCloneRehearser(nil, "", nil, nil, nil); r != nil || err != nil {
		t.Fatalf("no configuration: %v, %v", r, err)
	}
	dleCloneConfig(c)
	r, err := buildCloneRehearser(c, "", nil, nil, nil)
	if err != nil || r == nil {
		t.Fatalf("dle: %v, %v", r, err)
	}
	// An unreachable Database Lab: no clone, a distinguishable error that
	// does not carry the token.
	_, err = r.Rehearse(context.Background(), tuning.RehearsalRequest{
		DDL:        "CREATE INDEX CONCURRENTLY o_c ON public.orders (customer_id)",
		Statements: []tuning.RehearsalStatement{{QueryID: 1, Text: "SELECT 1"}}})
	if !errors.Is(err, tuning.ErrCloneUnavailable) || strings.Contains(err.Error(),
		"test-token") {
		t.Fatalf("rehearsal on an unreachable provider: %v", err)
	}
	c.Clone.Provider = "snapshot"
	if r, err := buildCloneRehearser(c, "", nil, nil, nil); r != nil || err == nil {
		t.Fatalf("an unsupported provider is an error: %v, %v", r, err)
	}
}

func TestNewTuningAgent_RehearseToolFollowsTheCloneProvider(t *testing.T) {
	for _, provider := range []string{"none", "dle"} {
		rt := standaloneDefaultsRuntime(t, "http://127.0.0.1:1/v1", "fixture-key")
		if provider == "dle" {
			dleCloneConfig(cfg)
		}
		agent := rt.newTuningAgent(nil)
		if agent == nil {
			t.Fatalf("%s: tuning on with a model builds the agent", provider)
		}
		offered := slices.Contains(agent.ToolNames(), "rehearse")
		if offered != (provider == "dle") {
			t.Fatalf("%s: rehearse offered = %v, tools %v", provider, offered,
				agent.ToolNames())
		}
		if (rt.floorRehearser() != nil) != (provider == "dle") {
			t.Fatalf("%s: floor rehearser %v", provider, rt.floorRehearser())
		}
		if rt.cloneRehearser() != rt.cloneRehearser() {
			t.Fatalf("%s: one rehearser per database, shared by agent and floor", provider)
		}
	}
}

type deadlineRehearser struct{ left time.Duration }

func (d *deadlineRehearser) Rehearse(ctx context.Context, _ tuning.RehearsalRequest) (
	tuning.RehearsalResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.left = time.Until(dl)
	}
	return tuning.RehearsalResult{BuildMs: 1}, ctx.Err()
}

// Every rehearsal, the tuning agent's included, ends within the
// rehearsal bound (a Lakebase branch's TTL when shorter than 1 h).
func TestCloneRehearser_EveryRehearsalIsBounded(t *testing.T) {
	inner := &deadlineRehearser{}
	res, err := boundedRehearser{inner: inner, bound: 5 * time.Minute}.Rehearse(
		context.Background(), tuning.RehearsalRequest{})
	if err != nil || res.BuildMs != 1 || inner.left <= 4*time.Minute ||
		inner.left > 5*time.Minute {
		t.Fatalf("bounded: left %s, %+v, %v", inner.left, res, err)
	}
	inner.left = 0
	if _, err := (boundedRehearser{inner: inner}).Rehearse(context.Background(),
		tuning.RehearsalRequest{}); err != nil || inner.left != 0 {
		t.Fatalf("no bound adds no deadline: left %s, %v", inner.left, err)
	}
	rt := standaloneDefaultsRuntime(t, "http://127.0.0.1:1/v1", "fixture-key")
	dleCloneConfig(cfg)
	b, ok := rt.cloneRehearser().(boundedRehearser)
	if !ok || b.bound != rehearsalMaxDuration(cfg.Clone) || b.bound <= 0 {
		t.Fatalf("the database's rehearser is bounded: %#v", rt.cloneRehearser())
	}
	f, ok := rt.floorRehearser().(floorRehearsal)
	if !ok || f.reh != rt.cloneRehearser() {
		t.Fatalf("the floor shares the bounded rehearser: %#v", rt.floorRehearser())
	}
}
