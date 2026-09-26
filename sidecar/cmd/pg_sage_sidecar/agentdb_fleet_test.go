package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func TestAgentDeploymentToFleetConfig_Eligible(t *testing.T) {
	dep := agentdb.Deployment{
		DeploymentID: "dep-123",
		Provider:     "local_postgres",
		TenantID:     "tenant-a",
		DatabaseName: "agent_db",
		Status:       "active",
		ConnectionInfo: map[string]any{
			"host":     "10.0.0.5",
			"port":     float64(5444), // JSON numbers arrive as float64
			"user":     "agent",
			"password": "secret",
			"database": "agent_db",
			"sslmode":  "require",
		},
	}
	cfg, ok := agentDeploymentToFleetConfig(dep)
	if !ok {
		t.Fatal("expected eligible deployment")
	}
	if cfg.Name != "agentdb:dep-123" {
		t.Errorf("Name = %q", cfg.Name)
	}
	if cfg.Host != "10.0.0.5" || cfg.Port != 5444 || cfg.Database != "agent_db" {
		t.Errorf("conn = %s:%d/%s", cfg.Host, cfg.Port, cfg.Database)
	}
	if cfg.SSLMode != "require" {
		t.Errorf("sslmode = %q", cfg.SSLMode)
	}
	if !cfg.HasTag("agentdb") || !cfg.HasTag("local_postgres") {
		t.Errorf("tags = %v", cfg.Tags)
	}
}

func TestAgentDeploymentToFleetConfig_Defaults(t *testing.T) {
	dep := agentdb.Deployment{
		DeploymentID: "d", Status: "active", DatabaseName: "db",
		ConnectionInfo: map[string]any{"host": "h", "database": "db"},
	}
	cfg, ok := agentDeploymentToFleetConfig(dep)
	if !ok {
		t.Fatal("expected eligible")
	}
	// G8-B21: remote agent databases never default to plaintext.
	if cfg.Port != 5432 || cfg.SSLMode != "require" {
		t.Errorf("defaults wrong: port=%d sslmode=%q", cfg.Port, cfg.SSLMode)
	}
}

func TestEligibleForFleet_Rejections(t *testing.T) {
	cases := []struct {
		name string
		dep  agentdb.Deployment
	}{
		{"inactive", agentdb.Deployment{Status: "archived",
			ConnectionInfo: map[string]any{"host": "h", "database": "d"}}},
		{"secret_ref present", agentdb.Deployment{Status: "active",
			ConnectionInfo: map[string]any{"host": "h", "database": "d", "secret_ref": "arn:..."}}},
		{"no host", agentdb.Deployment{Status: "active",
			ConnectionInfo: map[string]any{"database": "d"}}},
		{"nil conn info", agentdb.Deployment{Status: "active"}},
	}
	for _, c := range cases {
		if eligibleForFleet(c.dep) {
			t.Errorf("%s: expected ineligible", c.name)
		}
		if _, ok := agentDeploymentToFleetConfig(c.dep); ok {
			t.Errorf("%s: conversion should fail", c.name)
		}
	}
}

// G8-B21/SURF-07: fleet sync removes archived deployments and replaces an
// instance whose connection changed.
func TestAgentFleetPlanRemovesInactiveAndReplacesChanged(t *testing.T) {
	live := func(id, host string) agentdb.Deployment {
		return agentdb.Deployment{DeploymentID: id, Status: "active", DatabaseName: "db",
			ConnectionInfo: map[string]any{"host": host, "database": "db", "user": "u"}}
	}
	archived := live("gone", "h1")
	archived.Status = "archived"
	current := map[string]config.DatabaseConfig{
		"agentdb:gone":    {Name: "agentdb:gone", Host: "h1", Port: 5432, User: "u", Database: "db", SSLMode: "require"},
		"agentdb:moved":   {Name: "agentdb:moved", Host: "old", Port: 5432, User: "u", Database: "db", SSLMode: "require"},
		"agentdb:same":    {Name: "agentdb:same", Host: "h3", Port: 5432, User: "u", Database: "db", SSLMode: "require"},
		"agentdb:deleted": {Name: "agentdb:deleted", Host: "h4"},
	}
	add, remove := agentFleetPlan([]agentdb.Deployment{
		archived, live("moved", "new"), live("same", "h3"), live("fresh", "h5"),
	}, current)
	if strings.Join(remove, ",") != "agentdb:deleted,agentdb:gone,agentdb:moved" {
		t.Fatalf("remove = %v", remove)
	}
	names := []string{}
	for _, cfg := range add {
		names = append(names, cfg.Name+"@"+cfg.Host)
	}
	if strings.Join(names, ",") != "agentdb:fresh@h5,agentdb:moved@new" {
		t.Fatalf("add = %v", names)
	}
}

// G8-B18: any emergency-stopped fleet instance blocks AgentDB mutations.
func TestAgentDBMutationGateHonoursFleetEmergencyStop(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "prod", Stopped: true,
		Status: &fleet.InstanceStatus{}})
	if err := agentDBMutationGate(mgr)(context.Background()); err == nil {
		t.Fatal("gate allowed mutations during a fleet emergency stop")
	}
	if err := agentDBMutationGate(nil)(context.Background()); err != nil {
		t.Fatalf("nil manager gate: %v", err)
	}
}
