package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/store"
)

// No concurrent access tests here: the shared resolver's own locking is
// covered in internal/migration; this file checks the wiring.

// countResolvers swaps the resolver constructor for one that counts.
func countResolvers(t *testing.T) *int {
	t.Helper()
	n := 0
	previous := newMigrationStaleResolver
	newMigrationStaleResolver = func(
		pool *pgxpool.Pool, logFn func(string, string, ...any),
	) *migration.StaleResolver {
		n++
		return previous(pool, logFn)
	}
	t.Cleanup(func() { newMigrationStaleResolver = previous })
	return &n
}

func testMigrationAdvisor(mcfg *config.MigrationConfig) *migration.Advisor {
	return migration.NewAdvisor(nil, mcfg, 170000, "orders",
		func(string, string, ...any) {}, nil)
}

// wiringPool is a pool value the wiring can hold; nothing connects to it.
var wiringPool = &pgxpool.Pool{}

// State (migration-12): with activity polling and log detection both on,
// one database gets exactly one stale-finding resolver, shared by both
// detectors, so the resolve UPDATE runs once per interval, not twice.
func TestMigrationDetectorsShareOneResolver(t *testing.T) {
	n := countResolvers(t)
	findings := store.NewMigrationSafetyFindingStore(wiringPool)
	mcfg := &config.MigrationConfig{Enabled: true, ActivityPolling: true, LogDetection: true}
	detector, logDetector := newMigrationDetectors(
		wiringPool, testMigrationAdvisor(mcfg), findings, mcfg, true)
	if detector == nil || logDetector == nil {
		t.Fatalf("detector=%v logDetector=%v, want both", detector, logDetector)
	}
	if *n != 1 {
		t.Fatalf("constructed %d resolvers for one database, want 1", *n)
	}
	if detector.StaleResolver() == nil ||
		detector.StaleResolver() != logDetector.StaleResolver() {
		t.Fatalf("detectors hold different resolvers: %p vs %p",
			detector.StaleResolver(), logDetector.StaleResolver())
	}
}

// Boundary: one detector on still gets its resolver; none on builds
// neither a detector nor a resolver.
func TestMigrationDetectorsResolverPerEnabledSet(t *testing.T) {
	for _, tc := range []struct {
		name               string
		activity, logs     bool
		wantDet, wantLog   bool
		wantResolverBuilds int
	}{
		{"activity only", true, false, true, false, 1},
		{"logs only", false, true, false, true, 1},
		{"neither", false, false, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := countResolvers(t)
			findings := store.NewMigrationSafetyFindingStore(wiringPool)
			mcfg := &config.MigrationConfig{Enabled: true, ActivityPolling: tc.activity,
				LogDetection: tc.logs}
			detector, logDetector := newMigrationDetectors(
				wiringPool, testMigrationAdvisor(mcfg), findings, mcfg, tc.logs)
			if (detector != nil) != tc.wantDet || (logDetector != nil) != tc.wantLog {
				t.Fatalf("detector=%v logDetector=%v, want %v/%v",
					detector != nil, logDetector != nil, tc.wantDet, tc.wantLog)
			}
			if *n != tc.wantResolverBuilds {
				t.Fatalf("constructed %d resolvers, want %d", *n, tc.wantResolverBuilds)
			}
		})
	}
}
