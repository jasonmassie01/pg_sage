package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

type trustPersistCall struct {
	database string
	level    string
}

// metaTrustManager builds a meta-db style fleet: every database carries its
// own durable trust row (TrustLevelExplicit), as storeRecordToDBConfig does.
func metaTrustManager(t *testing.T, levels map[string]string) (
	*fleet.DatabaseManager, map[string]*executor.Executor,
) {
	t.Helper()
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	execs := map[string]*executor.Executor{}
	id := 100
	for name, level := range levels {
		id++
		exec := executor.New(nil, config.Clone(cfg), nil, time.Time{}, nil)
		if err := exec.SetTrustLevel(level); err != nil {
			t.Fatalf("seed %s trust: %v", name, err)
		}
		execs[name] = exec
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, DatabaseID: id, Executor: exec,
			Config: config.DatabaseConfig{
				Name: name, TrustLevel: level, TrustLevelExplicit: true,
			},
			Status: &fleet.InstanceStatus{TrustLevel: level},
		})
	}
	return mgr, execs
}

func metaTrustController(
	t *testing.T, mgr *fleet.DatabaseManager,
	persist func(context.Context, *fleet.DatabaseInstance, string) error,
) *config.ConfigController {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	return config.NewConfigController(cfg, nil, &trustPolicyOwner{
		manager: func() *fleet.DatabaseManager { return mgr },
		persist: persist,
	})
}

func applyGlobalTrust(
	t *testing.T, controller *config.ConfigController, level string,
) config.ApplyResult {
	t.Helper()
	candidate := controller.Desired().Config
	candidate.Trust.Level = level
	result, err := controller.Apply(
		context.Background(), controller.Desired().Generation, candidate,
	)
	if err != nil {
		t.Fatalf("apply trust %s: %v", level, err)
	}
	return result
}

// G5-B14: in meta-db mode every database has an explicit trust row, so a
// global trust downgrade used to reach no instance while reporting
// "applied". A downgrade must now cap every database, durably.
func TestMetaGlobalTrustDowngradeReachesEveryDatabase(t *testing.T) {
	mgr, execs := metaTrustManager(t, map[string]string{
		"orders": "autonomous", "billing": "advisory", "audit": "observation",
	})
	var calls []trustPersistCall
	controller := metaTrustController(t, mgr,
		func(_ context.Context, inst *fleet.DatabaseInstance, level string) error {
			calls = append(calls, trustPersistCall{inst.Name, level})
			return nil
		})

	result := applyGlobalTrust(t, controller, "advisory")

	want := map[string]string{
		"orders": "advisory", "billing": "advisory", "audit": "observation",
	}
	for name, level := range want {
		if got := execs[name].TrustLevel(); got != level {
			t.Errorf("%s executor trust = %q, want %q", name, got, level)
		}
		inst := mgr.GetInstance(name)
		if got := inst.SnapshotStatus().TrustLevel; got != level {
			t.Errorf("%s status trust = %q, want %q", name, got, level)
		}
	}
	if len(calls) != 1 || calls[0] != (trustPersistCall{"orders", "advisory"}) {
		t.Fatalf("persisted = %+v, want only orders->advisory", calls)
	}
	if result.ComponentStatus["trust_policy"] != "applied" {
		t.Fatalf("component status = %+v", result.ComponentStatus)
	}
}

// Raising the global level must never escalate a meta database past its own
// durable policy; the operator is told instead of silently ignored.
func TestMetaGlobalTrustRaiseNeverEscalatesAndWarns(t *testing.T) {
	mgr, execs := metaTrustManager(t, map[string]string{
		"orders": "observation", "billing": "advisory",
	})
	calls := 0
	controller := metaTrustController(t, mgr,
		func(context.Context, *fleet.DatabaseInstance, string) error {
			calls++
			return nil
		})
	applyGlobalTrust(t, controller, "observation")
	result := applyGlobalTrust(t, controller, "autonomous")

	if got := execs["orders"].TrustLevel(); got != "observation" {
		t.Fatalf("orders escalated to %q", got)
	}
	if got := execs["billing"].TrustLevel(); got != "observation" {
		t.Fatalf("billing = %q, want observation (capped by earlier downgrade)", got)
	}
	if len(result.Warnings) != 1 ||
		!strings.Contains(result.Warnings[0], "2 databases") ||
		!strings.Contains(result.Warnings[0], "not raised") {
		t.Fatalf("warnings = %v, want one 'not raised' warning for 2 databases",
			result.Warnings)
	}
	if calls != 1 {
		t.Fatalf("persist calls = %d, want 1 (only the downgrade of billing)", calls)
	}
}

// A durable write failure rolls back every database already lowered, in
// memory and in the store.
func TestMetaGlobalTrustPersistFailureRollsBack(t *testing.T) {
	mgr, execs := metaTrustManager(t, map[string]string{
		"a-db": "autonomous", "b-db": "autonomous",
	})
	var calls []trustPersistCall
	controller := metaTrustController(t, mgr,
		func(_ context.Context, inst *fleet.DatabaseInstance, level string) error {
			calls = append(calls, trustPersistCall{inst.Name, level})
			if inst.Name == "b-db" && level == "observation" {
				return errors.New("meta db unavailable")
			}
			return nil
		})

	result := applyGlobalTrust(t, controller, "observation")

	for name, exec := range execs {
		if got := exec.TrustLevel(); got != "autonomous" {
			t.Errorf("%s trust after rollback = %q, want autonomous", name, got)
		}
	}
	if result.ComponentStatus["trust_policy"] != "commit_failed" {
		t.Fatalf("component status = %+v, want commit_failed", result.ComponentStatus)
	}
	restored := trustPersistCall{"a-db", "autonomous"}
	if calls[len(calls)-1] != restored {
		t.Fatalf("persist calls = %+v, want durable restore of a-db last", calls)
	}
}

// YAML fleet keeps its contract: explicit per-database trust is untouched.
func TestYAMLFleetTrustOwnerHasNoMetaPersistence(t *testing.T) {
	prepareMetaGlobals(t)
	oldMeta := globalMetaState
	t.Cleanup(func() { globalMetaState = oldMeta })

	globalMetaState = nil
	if owner := newTrustPolicyOwner(); owner.persist != nil {
		t.Fatal("YAML fleet owner persists trust into monitored databases")
	}
	globalMetaState = &metaDBState{Pool: &pgxpool.Pool{}}
	if owner := newTrustPolicyOwner(); owner.persist == nil {
		t.Fatal("meta-db owner cannot persist a global trust downgrade")
	}
}
