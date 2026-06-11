package main

import (
	"testing"

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
	if cfg.Port != 5432 || cfg.SSLMode != "disable" {
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
