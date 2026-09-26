package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/startup"
)

// A fleet database that fails prerequisite checks (e.g. no
// pg_stat_statements) must stay monitored in degraded mode, as before the
// G5-B13 change, instead of being registered as failed — dropping it also
// skipped the first-admin bootstrap and locked the dashboard (TestFleet).
func TestInstanceChecksOrDegradedKeepsDatabaseMonitored(t *testing.T) {
	origChecks, origVersion := runInstanceChecks, detectInstanceVersion
	t.Cleanup(func() {
		runInstanceChecks, detectInstanceVersion = origChecks, origVersion
	})
	runInstanceChecks = func(context.Context, *pgxpool.Pool) (*startup.CheckResult, error) {
		return nil, errors.New("pg_stat_statements check: extension missing")
	}
	detectInstanceVersion = func(*pgxpool.Pool) int { return 170010 }

	checks, warn := instanceChecksOrDegraded(context.Background(), nil)
	if warn == nil {
		t.Fatal("prerequisite failure must be reported as a warning")
	}
	if checks == nil {
		t.Fatal("degraded checks must be returned so the database stays monitored")
	}
	if checks.PGVersionNum != 170010 || checks.HasWALColumns || checks.HasPlanTimeColumns {
		t.Fatalf("degraded capabilities wrong: %+v", *checks)
	}
}

func TestInstanceChecksOrDegradedPassesThroughSuccess(t *testing.T) {
	origChecks := runInstanceChecks
	t.Cleanup(func() { runInstanceChecks = origChecks })
	want := &startup.CheckResult{PGVersionNum: 160004, HasWALColumns: true}
	runInstanceChecks = func(context.Context, *pgxpool.Pool) (*startup.CheckResult, error) {
		return want, nil
	}
	checks, warn := instanceChecksOrDegraded(context.Background(), nil)
	if warn != nil || checks != want {
		t.Fatalf("successful checks must pass through: checks=%+v warn=%v", checks, warn)
	}
}
