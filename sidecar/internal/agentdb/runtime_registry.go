package agentdb

import (
	"context"
	"os"
	"strings"
	"time"
)

func RuntimeRunnerRegistryFromEnv(ctx context.Context) *RunnerRegistry {
	registry := DefaultRunnerRegistry()
	if os.Getenv("PG_SAGE_LIVE_PROVISIONING") != "1" {
		return registry
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	registerAWSRDSFromEnv(ctx, registry)
	registerCloudSQLFromEnv(registry)
	registerLakebaseFromEnv(registry)
	return registry
}

func registerAWSRDSFromEnv(ctx context.Context, registry *RunnerRegistry) {
	if os.Getenv("PG_SAGE_ENABLE_AWS_RDS_RUNNER") != "1" {
		return
	}
	region := firstNonEmpty(
		os.Getenv("PG_SAGE_AWS_REGION"),
		os.Getenv("AWS_REGION"),
		os.Getenv("AWS_DEFAULT_REGION"),
	)
	if region == "" {
		return
	}
	runner, err := NewAWSRDSRunnerFromDefaultConfig(ctx, region)
	if err == nil {
		registry.Register(runner)
	}
}

func registerCloudSQLFromEnv(registry *RunnerRegistry) {
	if os.Getenv("PG_SAGE_ENABLE_GCP_CLOUDSQL_RUNNER") != "1" {
		return
	}
	token := os.Getenv("PG_SAGE_GCP_ACCESS_TOKEN")
	project := firstNonEmpty(
		os.Getenv("PG_SAGE_GCP_PROJECT"),
		os.Getenv("GOOGLE_CLOUD_PROJECT"),
	)
	region := firstNonEmpty(os.Getenv("PG_SAGE_GCP_REGION"), "us-central1")
	if token == "" || project == "" {
		return
	}
	client := CloudSQLHTTPClient{
		TokenFunc: staticToken(token),
	}
	registry.Register(NewCloudSQLRunner(client, project, region))
}

func registerLakebaseFromEnv(registry *RunnerRegistry) {
	if os.Getenv("PG_SAGE_ENABLE_LAKEBASE_RUNNER") != "1" {
		return
	}
	host := firstNonEmpty(
		os.Getenv("PG_SAGE_DATABRICKS_HOST"),
		os.Getenv("DATABRICKS_HOST"),
	)
	token := firstNonEmpty(
		os.Getenv("PG_SAGE_DATABRICKS_TOKEN"),
		os.Getenv("DATABRICKS_TOKEN"),
	)
	if strings.TrimSpace(host) == "" || strings.TrimSpace(token) == "" {
		return
	}
	client := LakebaseHTTPClient{
		BaseURL:   host,
		TokenFunc: staticToken(token),
	}
	registry.Register(NewLakebaseRunner(client, false))
}

func staticToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		return token, nil
	}
}
