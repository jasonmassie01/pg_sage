package rca

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rca-08: only the cluster owner detects the cluster-wide snapshot
// signals; connection saturation counts client backends.

// clusterHotSnapshot fires every signal this file is about: client
// backends at 90% of max_connections, a 45 s replica lag, an hour-old
// prepared transaction and a long idle-in-transaction session.
func clusterHotSnapshot() *collector.Snapshot {
	now := time.Now()
	lag := "00:00:45.000000"
	addr := "10.0.0.2"
	return &collector.Snapshot{
		CollectedAt: now,
		System: collector.SystemStats{TotalBackends: 97, ClientBackends: 90,
			MaxConnections: 100, CacheHitRatio: 0.999,
			IdleInTransaction: 2, IdleInTxMaxSeconds: 900},
		Replication: &collector.ReplicationStats{Replicas: []collector.ReplicaInfo{
			{ClientAddr: &addr, ReplayLag: &lag}}},
		PreparedXacts: []collector.PreparedTransaction{{GID: "gx",
			Prepared: now.Add(-time.Hour), Database: "app", XIDAge: 1000}},
	}
}

func firedIDs(e *Engine, snap *collector.Snapshot) []string {
	var ids []string
	for _, s := range e.detectSignals(snap, nil, testConfig(), nil) {
		ids = append(ids, s.ID)
	}
	return ids
}

// Single-database mode is unchanged: a new engine owns its cluster and
// detects the cluster-wide signals.
func TestEngineOwnsClusterByDefault(t *testing.T) {
	eng := testEngine()
	if !eng.ClusterOwner() {
		t.Fatal("a new engine is not the cluster owner")
	}
	ids := firedIDs(eng, clusterHotSnapshot())
	for _, want := range []string{"connections_high", "replication_lag_increasing",
		"orphaned_prepared_tx", "idle_in_tx_elevated"} {
		if !slices.Contains(ids, want) {
			t.Errorf("owner fired %v, missing %s", ids, want)
		}
	}
}

// State transitions: a non-owner skips connections_high and replication
// lag but keeps its own database's signals; becoming owner again
// restores them.
func TestNonOwnerSkipsClusterWideSignals(t *testing.T) {
	eng := testEngine()
	eng.SetClusterOwner(false)
	ids := firedIDs(eng, clusterHotSnapshot())
	for _, skipped := range []string{"connections_high", "replication_lag_increasing"} {
		if slices.Contains(ids, skipped) {
			t.Errorf("non-owner fired %s (%v)", skipped, ids)
		}
	}
	// Prepared transactions and idle sessions are this database's rows.
	for _, kept := range []string{"orphaned_prepared_tx", "idle_in_tx_elevated"} {
		if !slices.Contains(ids, kept) {
			t.Errorf("non-owner lost per-database signal %s (%v)", kept, ids)
		}
	}
	eng.SetClusterOwner(true)
	if ids := firedIDs(eng, clusterHotSnapshot()); !slices.Contains(ids,
		"connections_high") || !slices.Contains(ids, "replication_lag_increasing") {
		t.Fatalf("owner again fired %v, want the cluster-wide signals back", ids)
	}
}

// Fleet: two engines of one cluster analyze the same saturated cluster;
// one opens the connections_high incident.
func TestFleetConnectionsHighOneIncident(t *testing.T) {
	owner, other := testEngine(), testEngine()
	other.SetClusterOwner(false)
	snap := clusterHotSnapshot()
	snap.Replication, snap.PreparedXacts, snap.ConfigData = nil, nil, nil
	n := 0
	for _, eng := range []*Engine{owner, other} {
		for _, inc := range eng.Analyze(snap, nil, testConfig(), nil) {
			if slices.Contains(inc.SignalIDs, "connections_high") {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("connections_high incidents across the fleet = %d, want 1", n)
	}
}

// Boundary: saturation counts client backends, not every backend, at the
// 80% threshold; a snapshot without the count falls back to total.
func TestConnectionsHighCountsClientBackends(t *testing.T) {
	eng := testEngine()
	cases := []struct {
		name          string
		total, client int
		fire          bool
	}{
		{"background workers do not saturate", 95, 40, false},
		{"one below threshold", 95, 79, false},
		{"at threshold", 80, 80, true},
		{"older snapshot falls back to total", 85, 0, true},
		{"older quiet snapshot", 10, 0, false},
	}
	for _, tc := range cases {
		snap := &collector.Snapshot{CollectedAt: time.Now(), System: collector.SystemStats{
			TotalBackends: tc.total, ClientBackends: tc.client, MaxConnections: 100}}
		sig := eng.detectConnectionsHigh(snap, nil, testConfig())
		if (sig != nil) != tc.fire {
			t.Errorf("%s: fired = %v, want %v", tc.name, sig != nil, tc.fire)
			continue
		}
		if sig == nil {
			continue
		}
		want := tc.client
		if want == 0 {
			want = tc.total
		}
		if sig.Metrics["client_backends"] != want || sig.Metrics["pct"] != want ||
			sig.Metrics["total_backends"] != tc.total {
			t.Errorf("%s: metrics = %v, want client_backends=pct=%d total=%d",
				tc.name, sig.Metrics, want, tc.total)
		}
	}
}

// Nil/empty: no max_connections fires nothing, owner or not.
func TestConnectionsHighZeroMaxConnections(t *testing.T) {
	eng := testEngine()
	snap := &collector.Snapshot{System: collector.SystemStats{ClientBackends: 5}}
	if sig := eng.detectConnectionsHigh(snap, nil, testConfig()); sig != nil {
		t.Fatalf("fired %+v without max_connections", sig)
	}
}

// Concurrency: ownership flips while cycles run.
func TestSetClusterOwnerDuringCycles(t *testing.T) {
	eng := testEngine()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			eng.SetClusterOwner(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			eng.Analyze(clusterHotSnapshot(), nil, testConfig(), nil)
		}
	}()
	wg.Wait()
	eng.SetClusterOwner(false)
	if eng.ClusterOwner() {
		t.Fatal("last SetClusterOwner(false) not kept")
	}
}

// stopCountingSource counts Stop calls.
type stopCountingSource struct {
	mu    sync.Mutex
	stops int
}

func (s *stopCountingSource) Start(context.Context) error { return nil }
func (s *stopCountingSource) Drain() []*Signal            { return nil }
func (s *stopCountingSource) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
}
func (s *stopCountingSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}

// CHECK-26: replacing a log source (provider sink wiring) stops the one
// it replaces, so no orphaned subscriber keeps filling; setting the same
// source again or the first source stops nothing.
func TestSetLogSourceStopsReplacedSource(t *testing.T) {
	eng := testEngine()
	first, second := &stopCountingSource{}, &stopCountingSource{}
	eng.SetLogSource(first)
	eng.SetLogSource(first)
	if first.count() != 0 {
		t.Fatalf("re-setting the same source stopped it %d times", first.count())
	}
	eng.SetLogSource(second)
	if first.count() != 1 || second.count() != 0 {
		t.Fatalf("stops first=%d second=%d, want 1/0", first.count(), second.count())
	}
	eng.SetLogSource(nil)
	if second.count() != 1 {
		t.Fatalf("clearing the source stopped it %d times, want 1", second.count())
	}
	eng.SetLogSource(nil)
	if first.count() != 1 || second.count() != 1 {
		t.Fatal("setting nil over nil stopped something")
	}
}
