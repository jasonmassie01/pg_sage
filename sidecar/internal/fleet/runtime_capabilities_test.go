package fleet

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeCapabilitiesNilPoolDoesNotInventEvidence(t *testing.T) {
	caps := CollectProviderCapabilities(context.Background(), nil, nil,
		"neon", "manual", false, time.Now())
	if caps.Extensions["vector"] != "unknown" || caps.ReadyForAutoSafe {
		t.Fatalf("nil pool invented capabilities: %#v", caps)
	}
}

// Read-only catalog probes; no schema or provider state is modified.
func TestRuntimeCapabilitiesInstalledExtensions(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL absent; runtime catalog probe not verified")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	caps := CollectProviderCapabilities(t.Context(), pool, nil,
		"supabase", "manual", false, time.Now())
	if caps.Extensions["plpgsql"] != "available" {
		t.Fatal("installed plpgsql extension was not detected")
	}
	for _, ext := range []string{"pg_stat_statements", "hypopg", "vector"} {
		var installed bool
		err := pool.QueryRow(t.Context(),
			"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname=$1)", ext).Scan(&installed)
		if err != nil {
			t.Fatalf("catalog query failed: %T", err)
		}
		want := "not_installed"
		if installed {
			want = "available"
		}
		if caps.Extensions[ext] != want {
			t.Errorf("%s = %q, want runtime state %q", ext, caps.Extensions[ext], want)
		}
	}
	if caps.Permissions["read_stats"].Status == "unknown" {
		t.Fatal("successful role privilege probe remained unknown")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stale := CollectProviderCapabilities(ctx, pool, nil, "supabase", "manual", false, time.Now())
	if stale.Extensions["vector"] != "unknown" || stale.ReadyForAutoSafe {
		t.Fatal("failed probe advertised known extension/readiness")
	}
}

// Extension installation alone cannot establish that a session module is loaded.
func TestRuntimeModuleState(t *testing.T) {
	for _, tc := range []struct {
		value *string
		want  string
	}{
		{nil, "installed_not_loaded"},
		{moduleSetting("off"), "installed_disabled"},
		{moduleSetting("on"), "available"},
	} {
		if got := hintModuleState(tc.value); got != tc.want {
			t.Errorf("module state %q, want %q", got, tc.want)
		}
	}
}

func moduleSetting(value string) *string { return &value }
