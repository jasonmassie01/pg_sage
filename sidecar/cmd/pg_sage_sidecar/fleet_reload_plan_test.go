package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: planFleetReload and validateFleetReloadPlan
// are pure functions of their arguments.

func planDB(name string) config.DatabaseConfig {
	return config.DatabaseConfig{
		Name: name, Host: name + ".internal", Port: 5432, User: "sage",
		Database: name, SSLMode: "disable", MaxConnections: 3,
		TrustLevel: "observation", ExecutionMode: "auto",
	}
}

func planNames(dbs []config.DatabaseConfig) []string {
	names := make([]string, 0, len(dbs))
	for _, db := range dbs {
		names = append(names, db.Name)
	}
	return names
}

func TestPlanFleetReloadEmptyInputs(t *testing.T) {
	for _, tc := range []struct {
		name            string
		active, desired []config.DatabaseConfig
	}{
		{"nil", nil, nil},
		{"empty", []config.DatabaseConfig{}, []config.DatabaseConfig{}},
		{"unchanged", []config.DatabaseConfig{planDB("a")},
			[]config.DatabaseConfig{planDB("a")}},
	} {
		plan := planFleetReload(tc.active, tc.desired)
		if !plan.Empty() {
			t.Errorf("%s: plan = %+v, want empty", tc.name, plan)
		}
	}
}

func TestPlanFleetReloadAddRemoveSorted(t *testing.T) {
	active := []config.DatabaseConfig{planDB("keep"), planDB("zeta"), planDB("alpha")}
	desired := []config.DatabaseConfig{planDB("keep"), planDB("new-b"), planDB("new-a")}
	plan := planFleetReload(active, desired)
	if got := planNames(plan.Add); !reflect.DeepEqual(got, []string{"new-a", "new-b"}) {
		t.Fatalf("add = %v", got)
	}
	if !reflect.DeepEqual(plan.Remove, []string{"alpha", "zeta"}) {
		t.Fatalf("remove = %v", plan.Remove)
	}
	if len(plan.Rebuild)+len(plan.Hot) != 0 || plan.Empty() {
		t.Fatalf("unexpected plan %+v", plan)
	}
}

func TestPlanFleetReloadRenameIsRemovePlusAdd(t *testing.T) {
	renamed := planDB("a")
	renamed.Name = "a2"
	plan := planFleetReload([]config.DatabaseConfig{planDB("a")},
		[]config.DatabaseConfig{renamed})
	if !reflect.DeepEqual(plan.Remove, []string{"a"}) ||
		!reflect.DeepEqual(planNames(plan.Add), []string{"a2"}) {
		t.Fatalf("rename plan = %+v", plan)
	}
}

func TestPlanFleetReloadClassifiesHotAndRebuild(t *testing.T) {
	hot := planDB("hot")
	hot.Tags = []string{"payments"}
	hot.TrustLevel = "advisory"
	rebuild := planDB("rebuild")
	rebuild.MaxConnections = 9
	both := planDB("both")
	both.Port = 6432
	both.ExecutionMode = "approval"
	active := []config.DatabaseConfig{planDB("hot"), planDB("rebuild"), planDB("both")}
	plan := planFleetReload(active, []config.DatabaseConfig{hot, rebuild, both})
	if got := planNames(plan.Hot); !reflect.DeepEqual(got, []string{"hot"}) {
		t.Fatalf("hot = %v", got)
	}
	if got := planNames(plan.Rebuild); !reflect.DeepEqual(got, []string{"both", "rebuild"}) {
		t.Fatalf("rebuild = %v", got)
	}
	if plan.Previous["hot"].TrustLevel != "observation" ||
		plan.Previous["both"].Port != 5432 {
		t.Fatalf("previous configs not retained for rollback: %+v", plan.Previous)
	}
	if plan.Hot[0].TrustLevel != "advisory" || plan.Rebuild[0].ExecutionMode != "approval" {
		t.Fatal("plan does not carry the desired configs")
	}
}

func TestValidateFleetReloadPlanProtectsControlDatabase(t *testing.T) {
	removeControl := planFleetReload(
		[]config.DatabaseConfig{planDB("ctl"), planDB("b")},
		[]config.DatabaseConfig{planDB("b")})
	if err := validateFleetReloadPlan(removeControl, "ctl"); !errors.Is(err, errFleetControlDatabase) {
		t.Fatalf("removing the control database = %v", err)
	}
	moved := planDB("ctl")
	moved.Host = "elsewhere"
	rebuildControl := planFleetReload([]config.DatabaseConfig{planDB("ctl")},
		[]config.DatabaseConfig{moved})
	if err := validateFleetReloadPlan(rebuildControl, "ctl"); !errors.Is(err, errFleetControlDatabase) {
		t.Fatalf("reconnecting the control database = %v", err)
	}
	retagged := planDB("ctl")
	retagged.Tags = []string{"primary"}
	hotControl := planFleetReload([]config.DatabaseConfig{planDB("ctl")},
		[]config.DatabaseConfig{retagged})
	if err := validateFleetReloadPlan(hotControl, "ctl"); err != nil {
		t.Fatalf("a hot change to the control database was refused: %v", err)
	}
	if err := validateFleetReloadPlan(removeControl, ""); err != nil {
		t.Fatalf("no control database recorded, yet refused: %v", err)
	}
}

func TestValidateFleetReloadPlanRejectsInvalidPolicyValues(t *testing.T) {
	badTrust := planDB("new")
	badTrust.TrustLevel = "autonomus"
	badMode := planDB("a")
	badMode.ExecutionMode = "yolo"
	for name, plan := range map[string]fleetReloadPlan{
		"added trust": planFleetReload(nil, []config.DatabaseConfig{badTrust}),
		"hot mode": planFleetReload([]config.DatabaseConfig{planDB("a")},
			[]config.DatabaseConfig{badMode}),
	} {
		if err := validateFleetReloadPlan(plan, ""); !errors.Is(err, errInvalidFleetDatabase) {
			t.Errorf("%s: err = %v, want errInvalidFleetDatabase", name, err)
		}
	}
	inherits := planDB("inherits")
	inherits.TrustLevel, inherits.ExecutionMode = "", ""
	if err := validateFleetReloadPlan(planFleetReload(nil,
		[]config.DatabaseConfig{inherits}), ""); err != nil {
		t.Fatalf("empty trust and mode inherit defaults, yet refused: %v", err)
	}
}
