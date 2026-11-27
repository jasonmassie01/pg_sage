package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestWatchFleetLearningPeers_RunsOnceWhenAPeerArrives(t *testing.T) {
	var peer atomic.Bool
	ran := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchFleetLearningPeers(ctx, 5*time.Millisecond, peer.Load,
			func(context.Context) { ran <- struct{}{} })
		close(done)
	}()
	waitRun := func(label string) {
		t.Helper()
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no cycle within the check period", label)
		}
	}
	quiet := func(label string) {
		t.Helper()
		select {
		case <-ran:
			t.Fatalf("%s: an extra cycle ran", label)
		case <-time.After(50 * time.Millisecond):
		}
	}
	quiet("still alone")
	peer.Store(true) // a second database registers
	waitRun("1 -> 2 sources")
	quiet("eligibility unchanged") // the hourly loop owns steady state
	peer.Store(false)
	quiet("back to one source")
	peer.Store(true)
	waitRun("2 sources again")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop when its context ended")
	}
}

func TestWatchFleetLearningPeers_EligibleAtStartIsLeftToTheLoop(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	watchFleetLearningPeers(ctx, 5*time.Millisecond, func() bool { return true },
		func(context.Context) { runs.Add(1) })
	if n := runs.Load(); n != 0 {
		t.Fatalf("an already eligible fleet triggered %d extra cycles", n)
	}
}

func bootstrapped(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("pool %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return pool
}

func TestRunFleetLearningCycle_StandaloneIdlesAndLogsOnce(t *testing.T) {
	db := bootstrapped(t, "fl_idle_db")
	prevSvc, prevLeader := fleetLearning, currentFleetLeader()
	t.Cleanup(func() { fleetLearning = prevSvc; setFleetLeader(prevLeader) })
	std := &config.Config{Mode: "standalone"}
	std.Postgres.Database = "app"
	mgr := fleet.NewManager(std)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "app", Pool: db,
		Status: &fleet.InstanceStatus{}})
	// Standalone: the monitored database is also the control database.
	setFleetLeader(nil)
	fleetLearning = fleetlearn.NewService(fleetlearn.NewStore(db, fleetLearningScope(std)),
		fleetLearningSources(mgr), fleetlearn.Settings{}, logStructuredWrapper)
	logs := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			runFleetLearningCycle(context.Background())
		}
	})
	if n := strings.Count(logs, "fleet learning: skipped, no peer in boundary"); n != 1 {
		t.Fatalf("3 standalone intervals logged the skip %d times, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs, "fingerprinted") {
		t.Fatalf("a skipped cycle reported fingerprinting:\n%s", logs)
	}
	var rows int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.fleet_fingerprint`).Scan(&rows); err != nil {
		t.Fatalf("count fingerprints: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a standalone sidecar stored %d fingerprints", rows)
	}
	if !strings.Contains(logs, "[INFO]") {
		t.Fatalf("the skip must be logged at INFO:\n%s", logs)
	}
}
