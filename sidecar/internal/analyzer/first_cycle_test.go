package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Five-minute time to value: the analyzer's first cycle runs as soon as
// the collector's first snapshot exists, not one analyzer interval (10 min
// by default) after startup.
func TestFirstCycleRunsWhenTheFirstSnapshotArrives(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Analyzer.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	coll := collector.New(pool, cfg, version, noopLog)
	a := New(pool, cfg, coll, nil, nil, nil, nil, noopLog)

	ctx, cancel := context.WithCancel(context.Background())
	analyzerDone := make(chan struct{})
	// The analyzer starts first: its first cycle must wait for the
	// snapshot instead of skipping and sleeping a full interval.
	go func() { a.Run(ctx); close(analyzerDone) }()
	collectorDone := make(chan struct{})
	go func() { coll.Run(ctx); close(collectorDone) }()

	deadline := time.Now().Add(30 * time.Second)
	for coll.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	snap := coll.LatestSnapshot()
	if snap == nil {
		cancel()
		t.Fatal("collector produced no startup snapshot")
	}
	time.Sleep(firstSnapshotPoll + 1500*time.Millisecond)
	cancel()
	<-analyzerDone
	<-collectorDone
	if a.lastAnalyzed != snap {
		t.Fatalf("analyzer did not analyze the startup snapshot within %s",
			firstSnapshotPoll+1500*time.Millisecond)
	}
}

func TestFirstSnapshotPollIsShort(t *testing.T) {
	if firstSnapshotPoll <= 0 || firstSnapshotPoll > 2*time.Second {
		t.Fatalf("firstSnapshotPoll = %s, want (0, 2s]", firstSnapshotPoll)
	}
}
