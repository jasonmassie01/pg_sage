package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: each detector here is private to its test.

// Neon and Supabase behind a custom domain are named by the last probe
// statement, which still answers azure first; no extra round trip.
func TestProviderProbeFallbacksForNeonAndSupabase(t *testing.T) {
	cases := []struct {
		name, fragment, value, want string
	}{
		{"neon GUC", "neon.tenant_id", "neon", "neon"},
		{"supabase_admin role", "supabase_admin", "supabase", "supabase"},
		{"azure still wins the statement", "azure.extensions", "azure", "azure"},
		{"any other value is azure", "azure.extensions", "pg_trgm", "azure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &scriptedProbeQuerier{answers: map[string]probeAnswer{
				"aurora_version": {err: undefinedFunction()},
				tc.fragment:      {value: textValue(tc.value)}}}
			got, err := newProviderDetector(q).probeOnce(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("probeOnce = %q, %v; want %q, nil", got, err, tc.want)
			}
			if n := q.callCount(); n != len(providerProbes) || n >= 6 {
				t.Fatalf("probe statements = %d, want %d (under 6)", n, len(providerProbes))
			}
		})
	}
}

// The fallbacks run in the azure statement, after it, in this order.
func TestProviderProbeFallbackStatementOrder(t *testing.T) {
	last := providerProbes[len(providerProbes)-1]
	if last.provider != "azure" {
		t.Fatalf("last probe = %q, want azure", last.provider)
	}
	azure := strings.Index(last.sql, "'azure.extensions'")
	neon := strings.Index(last.sql, "'neon.tenant_id'")
	supabase := strings.Index(last.sql, "'supabase_admin'")
	if azure < 0 || neon < azure || supabase < neon {
		t.Fatalf("fallback order in %q: azure %d, neon %d, supabase %d", last.sql,
			azure, neon, supabase)
	}
	if strings.Join(last.also, ",") != "neon,supabase" {
		t.Fatalf("fallback providers = %v, want [neon supabase]", last.also)
	}
}

// Integration: a database with a neon.* setting reads as Neon, as an
// ordinary role, in the same five statements.
func TestDetectCloudEnvNeonFallbackOnRealPostgres(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "probe_neon")
	admin := probeAdminPool(t)
	database := probeDatabaseName(t, dsn)
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(
		"ALTER DATABASE %q SET neon.tenant_id = 'b4f1c0de'", database)); err != nil {
		t.Fatalf("set neon.tenant_id: %v", err)
	}
	recorder := &testdb.QueryRecorder{}
	p := probePool(t, probeReaderDSN(t, dsn), recorder)
	recorder.Reset()

	got, err := detectCloudEnvContext(context.Background(), p)

	if got != "neon" || err != nil {
		t.Fatalf("detectCloudEnvContext = %q, %v; want neon, nil", got, err)
	}
	if n := len(recorder.Matching("")); n == 0 || n >= 6 {
		t.Fatalf("probe statements = %d, want 1..5", n)
	}
}

// Integration: a cluster with the supabase_admin role reads as Supabase.
func TestDetectCloudEnvSupabaseFallbackOnRealPostgres(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "probe_supabase")
	admin := probeAdminPool(t)
	ctx := context.Background()
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles "+
		"WHERE rolname = 'supabase_admin')").Scan(&exists); err != nil {
		t.Fatalf("read roles: %v", err)
	}
	if exists {
		t.Fatal("supabase_admin already exists on the test server; use a fresh server")
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE supabase_admin NOLOGIN"); err != nil {
		t.Fatalf("create supabase_admin: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			"DROP ROLE IF EXISTS supabase_admin"); err != nil {
			t.Errorf("drop supabase_admin: %v", err)
		}
	})
	p := probePool(t, probeReaderDSN(t, dsn), nil)
	detectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	got, err := detectCloudEnvContext(detectCtx, p)

	if got != "supabase" || err != nil {
		t.Fatalf("detectCloudEnvContext = %q, %v; want supabase, nil", got, err)
	}
}
