package main

import (
	"net/url"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func TestAgentFleetEnvironmentSecretReference(t *testing.T) {
	t.Setenv("SAGE_TEST_AGENT_DSN",
		"postgres://worker:synthetic@ep-test.neon.tech:5432/app?sslmode=require")
	dep := agentdb.Deployment{DeploymentID: "hosted", Provider: agentdb.ProviderNeon,
		Status: "active", SecretRef: "env://SAGE_TEST_AGENT_DSN",
		ConnectionInfo: map[string]any{"endpoint": "ep-test.neon.tech"}}
	got, ok := agentDeploymentToFleetConfig(dep)
	if !ok || got.Host != "ep-test.neon.tech" || got.Database != "app" ||
		got.User != "worker" || got.Password != "synthetic" || got.SSLMode != "require" {
		t.Fatal("environment DSN was not resolved into the runtime fleet config")
	}
	if _, exists := dep.ConnectionInfo["password"]; exists {
		t.Fatal("mutated persistent metadata")
	}
	for _, value := range []string{"", "not a DSN", "postgres://a:b@different.neon.tech/app",
		"postgres://a:b@ep-test.neon.tech/app?sslmode=disable"} {
		t.Setenv("SAGE_TEST_AGENT_DSN", value)
		if _, ok := agentDeploymentToFleetConfig(dep); ok {
			t.Fatal("unsafe or missing DSN accepted")
		}
	}
}

func TestAgentFleetSecretDatabaseAndConnectionOptions(t *testing.T) {
	dsn := "postgres://worker:synthetic@ep-test.neon.tech/app?sslmode=require&" +
		"application_name=hosted-test&connect_timeout=19"
	t.Setenv("SAGE_TEST_AGENT_DSN", dsn)
	dep := agentdb.Deployment{DeploymentID: "hosted", Provider: agentdb.ProviderNeon,
		Status: "active", SecretRef: "env:SAGE_TEST_AGENT_DSN", DatabaseName: "other",
		ConnectionInfo: map[string]any{"endpoint": "ep-test.neon.tech"}}
	if _, ok := agentDeploymentToFleetConfig(dep); ok {
		t.Fatal("same-host reference selected another deployment database")
	}
	dep.DatabaseName = "app"
	got, ok := agentDeploymentToFleetConfig(dep)
	if !ok {
		t.Fatal("matching reference rejected")
	}
	parsed, err := url.Parse(got.ConnString())
	if err != nil || parsed.Query().Get("application_name") != "hosted-test" ||
		parsed.Query().Get("connect_timeout") != "19" {
		t.Fatal("connection options were silently discarded")
	}
	got.Database = "new_target"
	parsed, err = url.Parse(got.ConnString())
	if err != nil || parsed.Path != "/new_target" || parsed.Query().Get("dbname") != "" {
		t.Fatal("preserved options override updated database identity")
	}
}
