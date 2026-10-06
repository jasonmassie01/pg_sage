package collector

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. A collection cycle writes its snapshots and its
// query_store samples to the store, creates the day partitions there, and
// writes no history to the monitored database.
func TestCollectorCycleWritesHistoryToTheStore(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	// A day past what bootstrap prepared: its partition must be made in the store.
	at := time.Now().UTC().AddDate(0, 0, 8)
	c := New(p.Monitored, config.DefaultConfig(), 170000, func(string, string, ...any) {})
	snap := &Snapshot{CollectedAt: at,
		Queries: []QueryStats{{QueryID: 61, Calls: 5, TotalExecTime: 50, MeanExecTime: 10}},
		System:  SystemStats{ActiveBackends: 2}}
	if err := c.persist(ctx, snap); err != nil {
		t.Fatalf("persist: %v", err)
	}
	c.recordQueryStore(ctx, snap)
	monSnaps, metaSnaps := p.Count(t, "snapshots")
	monQS, metaQS := p.Count(t, "query_store")
	if monSnaps != 0 || monQS != 0 {
		t.Fatalf("meta mode wrote history to the monitored database: %d snapshots, "+
			"%d samples", monSnaps, monQS)
	}
	if metaSnaps == 0 || metaQS != 1 {
		t.Fatalf("store holds %d snapshots and %d samples, want a cycle's rows and 1 sample",
			metaSnaps, metaQS)
	}
	day := at.Format("20060102")
	var inStore, inMonitored bool
	if err := p.Meta.QueryRow(ctx, `SELECT to_regclass('sage.snapshots_p' || $1) IS NOT NULL`,
		day).Scan(&inStore); err != nil || !inStore {
		t.Fatalf("the cycle's day partition must be ensured in the store (%v)", err)
	}
	if err := p.Monitored.QueryRow(ctx, `SELECT to_regclass('sage.snapshots_p' || $1)
		IS NOT NULL`, day).Scan(&inMonitored); err != nil || inMonitored {
		t.Fatalf("meta mode must not make history partitions in the monitored database (%v)",
			err)
	}
}

func TestCollectorCycleIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	got := map[histstore.Mode]string{}
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		c := New(p.Monitored, config.DefaultConfig(), 170000, func(string, string, ...any) {})
		snap := &Snapshot{CollectedAt: at,
			Queries: []QueryStats{{QueryID: 61, Calls: 5, TotalExecTime: 50, MeanExecTime: 10}},
			System:  SystemStats{ActiveBackends: 2, TotalBackends: 9}}
		if err := c.persist(ctx, snap); err != nil {
			t.Fatalf("%s persist: %v", mode, err)
		}
		var docs string
		if err := histstore.Resolve(p.Monitored).QueryRow(ctx, `SELECT string_agg(s.category
			|| '=' || sage.snapshot_data(s.data, s.base_id, s.collected_at)::text, ';'
			ORDER BY s.category) FROM sage.snapshots s WHERE {db:s}`).Scan(&docs); err != nil {
			t.Fatalf("%s read back: %v", mode, err)
		}
		got[mode] = docs
	}
	if got[histstore.ModeMonitored] == "" ||
		got[histstore.ModeMonitored] != got[histstore.ModeMeta] {
		t.Fatalf("documents differ:\nmonitored %s\nmeta      %s",
			got[histstore.ModeMonitored], got[histstore.ModeMeta])
	}
}
