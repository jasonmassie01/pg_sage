package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-08: the startup TRUST and FAST ELEVATION lines describe the
// effective configuration (after persisted overrides), not the file's.
//
// No concurrent-access tests: these run once on the startup path, before
// any goroutine reads the configuration.

// Without a control database the TRUST line still comes from the
// controller set-up, with the values that set-up publishes.
func TestControllerSetupLogsTrustMeaningAndFastElevation(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RampSafeHours = 1
	configController = nil
	out := captureStderr(t, func() {
		if err := initializeConfigController(nil); err != nil {
			t.Errorf("initializeConfigController: %v", err)
		}
	})
	if n := strings.Count(out, "TRUST: the ledger grants"); n != 1 {
		t.Fatalf("TRUST headline logged %d times, want once: %q", n, out)
	}
	if !strings.Contains(out, "trust.level=advisory") {
		t.Fatalf("TRUST line lacks the configured level: %q", out)
	}
	if !strings.Contains(out, "FAST ELEVATION: trust.ramp_safe_hours = 1") {
		t.Fatalf("fast-elevation warning missing: %q", out)
	}
	// A second call keeps the live controller and logs nothing again.
	again := captureStderr(t, func() {
		if err := initializeConfigController(nil); err != nil {
			t.Errorf("second initializeConfigController: %v", err)
		}
	})
	if strings.Contains(again, "TRUST:") {
		t.Fatalf("second set-up logged TRUST again: %q", again)
	}
}

// State transition: a trust.level saved through Settings (sage.config)
// replaces the file's value before the TRUST line is written.
func TestStartupTrustLineShowsThePersistedLevel(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	ctx := context.Background()
	configs := store.NewConfigStore(pool)
	if err := configs.SetOverride(ctx, "trust.level", "advisory", 0,
		fixtureAdminID(t, pool)); err != nil {
		t.Fatalf("persist trust.level=advisory: %v", err)
	}
	t.Cleanup(func() {
		if err := configs.DeleteOverride(ctx, "trust.level", 0); err != nil {
			t.Errorf("delete trust.level override: %v", err)
		}
	})
	cfg = config.DefaultConfig()
	configController = nil
	out := captureStderr(t, func() {
		if err := initializeConfigController(pool); err != nil {
			t.Errorf("initializeConfigController: %v", err)
		}
	})
	if cfg.Trust.Level != "advisory" {
		t.Fatalf("persisted override not applied: trust.level=%q", cfg.Trust.Level)
	}
	if !strings.Contains(out, "trust.level=advisory") ||
		strings.Contains(out, "trust.level="+config.DefaultTrustLevel) {
		t.Fatalf("TRUST line does not show the effective level: %q", out)
	}
}

// fixtureAdminID returns the package fixtures' admin, creating it on an
// empty users table; metaLifecycleFixture finds or creates the same admin
// by email.
func fixtureAdminID(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := context.Background()
	count, err := auth.UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count == 0 {
		if err := auth.BootstrapAdmin(ctx, pool, adminEmail, "fixture-admin-password"); err != nil {
			t.Fatalf("bootstrap fixture admin: %v", err)
		}
	}
	var id int
	err = pool.QueryRow(ctx, "SELECT id FROM sage.users WHERE email=$1", adminEmail).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return overrideAuthor(t, pool)
	}
	if err != nil {
		t.Fatalf("find fixture admin: %v", err)
	}
	return id
}
