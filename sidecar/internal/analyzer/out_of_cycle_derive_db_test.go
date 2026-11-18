package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// fleetpol-02a review: an out-of-cycle run's findings are rewritten as a
// cycle rewrites them (confirmed facts, clone-schema families, the rule
// pack) on the latest snapshot, so a sweep never reports what the next
// full cycle would fold away and resolve, and the derived categories
// stay the cycle's.

// snapshotAnalyzer is a lifecycle analyzer whose collector has taken its
// startup snapshot.
func snapshotAnalyzer(t *testing.T) (*Analyzer, *pgxpool.Pool) {
	t.Helper()
	a, pool, _ := lifecycleAnalyzer(t)
	a.cfg.Safety.CPUCeilingPct = 100
	a.cfg.Collector.IntervalSeconds = 3600
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	coll := collector.New(pool, a.cfg, version, noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coll.Run(ctx); close(done) }()
	deadline := time.Now().Add(30 * time.Second)
	for coll.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if coll.LatestSnapshot() == nil {
		t.Fatal("collector produced no startup snapshot")
	}
	a.collector = coll
	return a, pool
}

func TestRunOutOfCycle_AppliesTheCycleDerivationsToItsOwnCategories(t *testing.T) {
	a, pool := snapshotAnalyzer(t)
	ctx := context.Background()
	kept := lifecycleFinding("test_phase2_oc_posture", "public.leads", "critical")
	folded := lifecycleFinding("test_phase2_oc_posture", "fixture_a.leads", "critical")
	derived := lifecycleFinding("test_phase2_oc_cleanup", "fixture_*", "info")
	// An open finding of the derived category, from an earlier cycle.
	older := lifecycleFinding("test_phase2_oc_cleanup", "older_*", "info")
	a.eval = newCycleEval()
	if err := a.finalizeCycle(ctx, []Finding{older},
		map[string]bool{older.Category: true}); err != nil {
		t.Fatalf("seed the derived category: %v", err)
	}
	a.WithFactFilter(&recordingFactFilter{excluded: []string{"fixture_a"},
		out: []Finding{kept, derived}, evaluated: []string{derived.Category}})

	err := a.RunOutOfCycle(ctx, outOfCycle([]Finding{kept, folded},
		[]string{kept.Category}, nil))
	if err != nil {
		t.Fatalf("RunOutOfCycle: %v", err)
	}
	if got := countFindingRows(t, pool, kept.Category, kept.ObjectIdentifier,
		"open"); got != 1 {
		t.Fatalf("kept finding open rows = %d, want 1", got)
	}
	if got := countCategoryRows(t, pool, kept.Category); got != 1 {
		t.Fatalf("%s rows = %d, want 1: the finding the facts fold away was "+
			"published", kept.Category, got)
	}
	if got := countFindingRows(t, pool, derived.Category, derived.ObjectIdentifier,
		"open"); got != 0 {
		t.Fatalf("derived finding rows = %d, want 0: derived categories are the "+
			"cycle's", got)
	}
	if got := countFindingRows(t, pool, older.Category, older.ObjectIdentifier,
		"open"); got != 1 {
		t.Fatalf("older derived finding open rows = %d, want 1: an out-of-cycle run "+
			"resolved a category it does not own", got)
	}
	// The cycle's finding of the derived category stays; of the run's, only
	// the one the facts keep.
	latest := a.LatestFindings()
	if len(latest) != 2 || latest[0].ObjectIdentifier != older.ObjectIdentifier ||
		latest[1].ObjectIdentifier != kept.ObjectIdentifier {
		t.Fatalf("latest findings = %+v, want %s then %s", latest,
			older.ObjectIdentifier, kept.ObjectIdentifier)
	}
}
