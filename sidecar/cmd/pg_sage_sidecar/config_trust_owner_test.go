package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestTrustPolicyOwnerUpdatesInheritedExecutor(t *testing.T) {
	cfg := config.DefaultConfig()
	exec := executor.New(nil, cfg, nil, time.Time{}, nil)
	mgr := fleet.NewManager(cfg)
	inst := &fleet.DatabaseInstance{
		Name: "inherited", Executor: exec,
		Status: &fleet.InstanceStatus{TrustLevel: "observation"},
	}
	mgr.RegisterInstance(inst)
	owner := &trustPolicyOwner{manager: func() *fleet.DatabaseManager { return mgr }}
	desired := config.ConfigSnapshot{Generation: 2, Config: config.Clone(cfg)}
	desired.Config.Trust.Level = "advisory"
	prepared, err := owner.Prepare(context.Background(),
		config.ConfigSnapshot{Generation: 1, Config: cfg}, desired)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := prepared.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := exec.TrustLevel(); got != "advisory" {
		t.Fatalf("executor trust = %q, want advisory", got)
	}
}

func TestTrustPolicyOwnerSkipsExplicitDatabaseAndFleetDefaultPolicy(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"database", "fleet-default"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name,
			Config: config.DatabaseConfig{
				TrustLevel: "autonomous", TrustLevelExplicit: true,
			},
			Status: &fleet.InstanceStatus{TrustLevel: "autonomous"},
		})
	}
	owner := &trustPolicyOwner{manager: func() *fleet.DatabaseManager { return mgr }}
	desired := config.ConfigSnapshot{Generation: 2, Config: config.Clone(cfg)}
	desired.Config.Trust.Level = "advisory"
	prepared, err := owner.Prepare(context.Background(),
		config.ConfigSnapshot{Generation: 1, Config: cfg}, desired)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := prepared.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, inst := range mgr.Instances() {
		if got := inst.SnapshotStatus().TrustLevel; got != "autonomous" {
			t.Fatalf("%s trust = %q, want autonomous", inst.Name, got)
		}
	}
}

func TestTrustPolicyOwnerRollsBackPartiallyCommittedTargets(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"a", "b"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Status: &fleet.InstanceStatus{TrustLevel: "observation"},
		})
	}
	owner := &trustPolicyOwner{
		manager: func() *fleet.DatabaseManager { return mgr },
		apply: func(inst *fleet.DatabaseInstance, level string) error {
			if inst.Name == "b" && level == "advisory" {
				return errors.New("injected apply failure")
			}
			inst.UpdateStatus(func(status *fleet.InstanceStatus) {
				status.TrustLevel = level
			})
			return nil
		},
	}
	desired := config.ConfigSnapshot{Generation: 2, Config: config.Clone(cfg)}
	desired.Config.Trust.Level = "advisory"
	prepared, err := owner.Prepare(context.Background(),
		config.ConfigSnapshot{Generation: 1, Config: cfg}, desired)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := prepared.Commit(context.Background()); err == nil {
		t.Fatal("partial commit unexpectedly succeeded")
	}
	if err := prepared.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, inst := range mgr.Instances() {
		if got := inst.SnapshotStatus().TrustLevel; got != "observation" {
			t.Fatalf("%s trust after rollback = %q", inst.Name, got)
		}
	}
}
