package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/startup"
)

func TestBuildDatabaseRuntimeRejectsIncompleteSpec(t *testing.T) {
	for name, spec := range map[string]databaseRuntimeSpec{
		"nil pool":   {Name: "orders"},
		"empty name": {Pool: &pgxpool.Pool{}},
		"zero spec":  {},
	} {
		t.Run(name, func(t *testing.T) {
			rt, err := buildDatabaseRuntime(context.Background(), spec)
			if err == nil || rt != nil {
				t.Fatalf("incomplete spec built runtime=%v err=%v", rt, err)
			}
			if !strings.Contains(err.Error(), "pool and a name") {
				t.Fatalf("error %q does not name the missing input", err)
			}
		})
	}
}

// stubPreparation replaces the schema bootstrap and prerequisite probes.
func stubPreparation(t *testing.T, checks *startup.CheckResult, checkErr error) *int {
	t.Helper()
	oldBootstrap, oldChecks, oldVersion :=
		bootstrapManagedDatabaseSchema, runInstanceChecks, detectInstanceVersion
	t.Cleanup(func() {
		bootstrapManagedDatabaseSchema, runInstanceChecks, detectInstanceVersion =
			oldBootstrap, oldChecks, oldVersion
	})
	bootstraps := 0
	bootstrapManagedDatabaseSchema = func(context.Context, *pgxpool.Pool) error {
		bootstraps++
		return nil
	}
	runInstanceChecks = func(context.Context, *pgxpool.Pool) (*startup.CheckResult, error) {
		return checks, checkErr
	}
	detectInstanceVersion = func(*pgxpool.Pool) int { return 160004 }
	return &bootstraps
}

func TestPrepareMonitoredDatabaseRequiredChecksRefuse(t *testing.T) {
	wantErr := errors.New("pg_stat_statements is not installed")
	bootstraps := stubPreparation(t, nil, wantErr)
	checks, err := prepareMonitoredDatabase(context.Background(), nil, "orders", true)
	if !errors.Is(err, wantErr) || checks != nil {
		t.Fatalf("required checks: checks=%v err=%v, want %v", checks, err, wantErr)
	}
	if !strings.Contains(err.Error(), `"orders"`) {
		t.Fatalf("error %q does not name the database", err)
	}
	if *bootstraps != 1 {
		t.Fatalf("schema bootstrap ran %d times, want 1", *bootstraps)
	}
}

func TestPrepareMonitoredDatabaseDegradesOptionalChecks(t *testing.T) {
	stubPreparation(t, nil, errors.New("permission denied for pg_stat_statements"))
	checks, err := prepareMonitoredDatabase(context.Background(), nil, "orders", false)
	if err != nil {
		t.Fatalf("degradable checks refused the database: %v", err)
	}
	if checks.PGVersionNum != 160004 || checks.HasWALColumns || checks.HasPlanTimeColumns {
		t.Fatalf("degraded checks = %+v, want probed version and capabilities off", checks)
	}
}

func TestPrepareMonitoredDatabasePassesCheckResultThrough(t *testing.T) {
	want := &startup.CheckResult{PGVersionNum: 170002, HasWALColumns: true,
		QueryTextVisible: true}
	stubPreparation(t, want, nil)
	checks, err := prepareMonitoredDatabase(context.Background(), nil, "orders", true)
	if err != nil || checks != want {
		t.Fatalf("checks=%+v err=%v, want %+v", checks, err, want)
	}
}

func TestRuntimePrerequisitesSkipWorkTheCallerDid(t *testing.T) {
	bootstraps := stubPreparation(t, nil, errors.New("must not run"))
	done := &startup.CheckResult{PGVersionNum: 180000}
	checks, err := runtimePrerequisites(context.Background(),
		databaseRuntimeSpec{Name: "orders", Checks: done, RequireChecks: true})
	if err != nil || checks != done || *bootstraps != 0 {
		t.Fatalf("checks=%v err=%v bootstraps=%d", checks, err, *bootstraps)
	}
}

func TestRuntimeConfigIsolatesNonSharedModes(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	checks := &startup.CheckResult{PGVersionNum: 170002, HasWALColumns: true}
	rt := &databaseRuntime{checks: checks, provider: "rds"}
	isolated := rt.runtimeConfig()
	if isolated == cfg || isolated.CloudEnvironment != "rds" ||
		isolated.PGVersionNum != 170002 || !isolated.HasWALColumns {
		t.Fatalf("fleet runtime config = %+v", isolated)
	}
	if cfg.CloudEnvironment == "rds" || cfg.HasWALColumns {
		t.Fatal("a fleet database's flags leaked into the process config")
	}
	rt.spec.Shared = true
	if shared := rt.runtimeConfig(); shared != cfg || cfg.CloudEnvironment != "rds" ||
		cfg.PGVersionNum != 170002 {
		t.Fatal("standalone runtime does not share the live process config")
	}
}

func TestEnsureAnalyzeSemaphoreIsSharedUnderConcurrency(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Tuner.MaxConcurrentAnalyze = 2
	analyzeSem = nil
	results := make([]chan struct{}, 16)
	var workers sync.WaitGroup
	for i := range results {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results[i] = ensureAnalyzeSemaphore()
		}()
	}
	workers.Wait()
	for _, sem := range results {
		if sem == nil || sem != results[0] || cap(sem) != 2 {
			t.Fatal("concurrent runtimes received different or unsized semaphores")
		}
	}
	initializeAnalyzeSemaphore()
	if ensureAnalyzeSemaphore() == results[0] {
		t.Fatal("mode startup did not resize the process semaphore")
	}
}
