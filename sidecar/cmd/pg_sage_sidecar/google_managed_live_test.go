//go:build cloudlive

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Owner-run Cloud SQL and AlloyDB acceptance root (clouds-12; never in CI,
// which only compiles the cloudlive tag). Point the DSN variables at a
// disposable instance through the Auth Proxy or a private IP, connected as
// the sage_agent role. The session is read-only with statement and lock
// timeouts, so nothing on the instance is modified. The Cloud Monitoring
// sample is TestLiveGCPTelemetry in internal/cloudtel. See
// docs/managed-clouds.md "Live check".

// No concurrent access, state transition or fake-backed cases here: this
// root asserts only what a real Google-managed server answers.

func TestLiveGoogleManagedIdentityAndReadiness(t *testing.T) {
	for _, tc := range []struct{ provider, env string }{
		{"cloud-sql", "SAGE_TEST_CLOUDSQL_DATABASE_URL"},
		{"alloydb", "SAGE_TEST_ALLOYDB_DATABASE_URL"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip(tc.env + " absent; " + tc.provider +
					" identity and readiness NOT verified live")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			pool := liveReadOnlyPool(ctx, t, dsn)
			checkLiveIdentity(ctx, t, pool, tc.provider)
			checkLiveReadiness(ctx, t, pool, tc.provider)
		})
	}
}

func liveReadOnlyPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse live DSN: %v", err)
	}
	cfg.MinConns, cfg.MaxConns = 0, 2
	params := cfg.ConnConfig.RuntimeParams
	params["default_transaction_read_only"] = "on"
	params["statement_timeout"] = "5000"
	params["lock_timeout"] = "1000"
	params["application_name"] = "pg_sage_google_managed_live"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to the live instance: %v", err)
	}
	t.Cleanup(pool.Close)
	var readOnly string
	if err := pool.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&readOnly); err != nil {
		t.Fatalf("verify read-only session: %v", err)
	}
	if readOnly != "on" {
		t.Fatalf("live session is not read-only (%q); refusing to continue", readOnly)
	}
	return pool
}

// checkLiveIdentity is the provider-identity guard: the product's own
// probes must name the expected provider, so a DSN pointing at the wrong
// server (or self-managed PostgreSQL) fails instead of passing vacuously.
func checkLiveIdentity(ctx context.Context, t *testing.T, pool *pgxpool.Pool, want string) {
	t.Helper()
	got, err := newProviderDetector(pool).detect(ctx)
	if err != nil {
		t.Fatalf("CHECK-GCP-01: FAIL provider probe: %v", err)
	}
	if got != want {
		t.Fatalf("CHECK-GCP-01: FAIL detected %q, want %q: wrong instance or probe drift",
			got, want)
	}
	var version string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("read server_version: %v", err)
	}
	t.Logf("CHECK-GCP-01: PASS detected %s (PostgreSQL %s)", got, version)
}

// checkLiveReadiness collects runtime capabilities with no standing gate:
// families the provider adapter does not support must be unsupported, and
// without an explainer no family may execute (fail closed).
func checkLiveReadiness(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provider string) {
	t.Helper()
	caps := fleet.CollectProviderCapabilities(ctx, pool, provider, nil)
	if caps.Provider != provider || len(caps.ActionFamilies) == 0 {
		t.Fatalf("CHECK-GCP-02: FAIL readiness provider=%q families=%d",
			caps.Provider, len(caps.ActionFamilies))
	}
	adapter := fleet.AdapterForProvider(provider)
	for _, family := range caps.ActionFamilies {
		if family.Supported && !adapter.SupportsAction(family.ActionType) {
			t.Errorf("CHECK-GCP-02: FAIL %s supported but the %s adapter refuses it",
				family.ActionType, provider)
		}
		if family.Decision == executor.PolicyDecisionExecute {
			t.Errorf("CHECK-GCP-02: FAIL %s executes without a standing gate",
				family.ActionType)
		}
	}
	if caps.ReadyForAutoSafe {
		t.Error("CHECK-GCP-02: FAIL ready for auto-safe without a standing gate")
	}
	t.Logf("CHECK-GCP-02: PASS %d families, replica=%t, extensions=%v, blockers=%v",
		len(caps.ActionFamilies), caps.IsReplica, caps.Extensions, caps.Blockers)
}
