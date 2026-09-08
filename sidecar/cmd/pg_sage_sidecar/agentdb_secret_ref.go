package main

import (
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/config"
)

func agentDeploymentSecretRef(dep agentdb.Deployment) string {
	if dep.SecretRef != "" {
		return dep.SecretRef
	}
	return mapString(dep.ConnectionInfo, "secret_ref")
}

// Credentials are resolved only in memory; persistent deployment metadata retains the reference.
func resolveAgentEnvironmentSecret(dep agentdb.Deployment) (config.DatabaseConfig, bool) {
	ref := agentDeploymentSecretRef(dep)
	if dep.Status != "active" || !strings.HasPrefix(ref, "env:") ||
		(dep.SecretRefExpiresAt != nil && time.Now().After(*dep.SecretRefExpiresAt)) {
		return config.DatabaseConfig{}, false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(ref, "env:"), "//")
	dsn := os.Getenv(name)
	parsed, err := pgx.ParseConfig(dsn)
	uri, uriErr := url.Parse(dsn)
	if dsn == "" || err != nil || uriErr != nil ||
		(uri.Scheme != "postgres" && uri.Scheme != "postgresql") {
		return config.DatabaseConfig{}, false
	}
	host := mapString(dep.ConnectionInfo, "host")
	if host == "" {
		host = mapString(dep.ConnectionInfo, "endpoint")
	}
	if host == "" || !strings.EqualFold(host, parsed.Host) || parsed.Database == "" {
		return config.DatabaseConfig{}, false
	}
	if expected := agentDBName(dep); expected != "" && expected != parsed.Database {
		return config.DatabaseConfig{}, false
	}
	sslmode := uri.Query().Get("sslmode")
	if dep.Provider == agentdb.ProviderNeon || dep.Provider == agentdb.ProviderSupabase {
		if sslmode != "require" && sslmode != "verify-ca" && sslmode != "verify-full" {
			return config.DatabaseConfig{}, false
		}
	}
	if sslmode == "" {
		sslmode = "prefer"
	}
	resolved := config.DatabaseConfig{Name: agentFleetPrefix + dep.DeploymentID,
		Host: parsed.Host, Port: int(parsed.Port), User: parsed.User, Password: parsed.Password,
		Database: parsed.Database, SSLMode: sslmode,
		Tags: []string{"agentdb", dep.Provider, dep.TenantID}}
	resolved.SetRuntimeConnectionOptions(uri.Query())
	return resolved, true
}
