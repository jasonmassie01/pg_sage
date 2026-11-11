package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
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
