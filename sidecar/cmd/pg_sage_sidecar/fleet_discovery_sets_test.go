package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01b wiring: the grades discovery asks for and reads, the Neon
// client and the /metrics hook. No concurrent access tests here: the
// hub and the runner are tested concurrently in their packages.

func TestMetaDiscoveryGrades_NotReadyUntilTheMemberRuns(t *testing.T) {
	g := metaDiscoveryGrades{hub: trigger.NewHub(),
		pool: func(string) *pgxpool.Pool { return nil }}
	row := store.DiscoveredRow{ID: 3, Source: "cluster", ExternalID: "h:5432/app"}
	err := g.RequestFirstGrade(context.Background(), "cluster-app-3", row)
	if !errors.Is(err, discovery.ErrMemberNotReady) ||
		!strings.Contains(err.Error(), "cluster-app-3") {
		t.Fatalf("unregistered member: %v, want ErrMemberNotReady naming it", err)
	}
	if _, err := g.Grades(context.Background(), "cluster-app-3"); !errors.Is(err,
		discovery.ErrMemberNotReady) {
		t.Fatalf("member without a pool: %v, want ErrMemberNotReady", err)
	}
	h := trigger.NewHub()
	h.Register("cluster-app-3", trigger.NewStore(nil), 1<<30)
	g.hub = h
	err = g.RequestFirstGrade(context.Background(), "cluster-app-3", row)
	if err == nil || errors.Is(err, discovery.ErrMemberNotReady) ||
		!errors.Is(err, trigger.ErrNoStore) {
		t.Fatalf("queue without a pool: %v, want the store error, not not-ready", err)
	}
}

// Integration: a member's review-01 grades become discovery grades,
// oldest first, with a letter per dimension.
func TestMetaDiscoveryGrades_ReadsTheMembersGrades(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	member := "cluster-grades-" + time.Now().Format("150405.000000")
	s := rubric.NewStore(pool)
	for _, letter := range []rubric.Letter{"C", "A"} {
		id, err := s.Start(ctx, rubric.RunSpec{Database: member, Kind: rubric.KindGrade,
			Triggers: []string{"first_look"}, RubricVersion: rubric.Version})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := s.Finish(ctx, id, rubric.RunDone, &rubric.Grade{
			RubricVersion: rubric.Version, Overall: letter, OverallScore: 75,
			CoveragePct: 100, Dimensions: []rubric.DimensionGrade{{Dimension: "safety",
				Title: "Safety", Letter: letter, Score: 75, Coverage: 100}}}, ""); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
	g := metaDiscoveryGrades{hub: trigger.NewHub(),
		pool: func(string) *pgxpool.Pool { return pool }}
	got, err := g.Grades(ctx, member)
	if err != nil || len(got) != 2 || got[0].Letters["safety"] != "C" ||
		got[1].Letters["safety"] != "A" || got[0].At.IsZero() || got[1].At.Before(got[0].At) {
		t.Fatalf("grades = %+v %v, want C then A", got, err)
	}
}

func TestNeonDiscoveryAPI_NeedsTheCloneKey(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = config.DefaultConfig()
	cfg.Clone.Neon.APIKey = ""
	if _, err := neonDiscoveryAPI(); err == nil ||
		!strings.Contains(err.Error(), "clone.neon.api_key") {
		t.Fatalf("no key: %v, want an error naming clone.neon.api_key", err)
	}
	cfg.Clone.Neon.APIKey = "test-key"
	if api, err := neonDiscoveryAPI(); err != nil || api == nil {
		t.Fatalf("with a key: %v %v", api, err)
	}
}

func TestWriteFleetEnrollMetrics_NothingWithoutARunner(t *testing.T) {
	saved := fleetDiscoveryRunner.Load()
	t.Cleanup(func() { fleetDiscoveryRunner.Store(saved) })
	fleetDiscoveryRunner.Store(nil)
	var b strings.Builder
	writeFleetEnrollMetrics(&b)
	if b.Len() != 0 {
		t.Fatalf("metrics without a runner: %q", b.String())
	}
	r, err := discovery.NewRunner(config.FleetDiscoveryConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	fleetDiscoveryRunner.Store(r)
	writeFleetEnrollMetrics(&b)
	if b.Len() != 0 {
		t.Fatalf("metrics before a cycle: %q", b.String())
	}
}
