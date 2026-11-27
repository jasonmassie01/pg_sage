package main

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/databricksauth"
)

// Startup must refuse an unverified Lakebase OAuth connection with the
// actionable error intact; main exits non-zero on any error from these.
func TestStartupPoolsRefuseUnverifiedLakebaseTLS(t *testing.T) {
	// Keep any regression away from a real workspace: fake token, local host.
	t.Setenv("DATABRICKS_CONFIG_FILE", t.TempDir()+"/absent.cfg")
	t.Setenv("DATABRICKS_CONFIG_PROFILE", "")
	t.Setenv("DATABRICKS_HOST", "https://127.0.0.1:1")
	t.Setenv("DATABRICKS_AUTH_TYPE", "pat")
	t.Setenv("DATABRICKS_TOKEN", "test-not-a-real-token")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "projects/p/branches/b/endpoints/e")
	const dsn = "postgres://test@db.example/test?sslmode=require"
	opens := map[string]func() (*pgxpool.Pool, error){
		"meta":      func() (*pgxpool.Pool, error) { return connectMetaDB(dsn) },
		"monitored": func() (*pgxpool.Pool, error) { return connectMonitoredDB(dsn, 3) },
	}
	for name, open := range opens {
		t.Run(name, func(t *testing.T) {
			pool, err := open()
			if pool != nil {
				pool.Close()
				t.Fatal("startup opened a pool over unverified TLS")
			}
			if !errors.Is(err, databricksauth.ErrUnverifiedTLS) {
				t.Fatalf("startup error = %v; want ErrUnverifiedTLS unchanged", err)
			}
		})
	}
}
