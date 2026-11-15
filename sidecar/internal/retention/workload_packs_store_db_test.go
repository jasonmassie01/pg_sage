package retention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-02a: stored workload evidence packs live where the history lives.
// In meta mode the history store's cleaner ages every database's packs out
// of the meta database, and a per-database cleaner ages the packs left in
// its monitored database. No concurrency test: one cleaner runs per store.

func packCfg() *config.Config {
	cfg := storeCfg()
	cfg.Retention.WorkloadPacksDays = 180
	return cfg
}

func insertPack(t *testing.T, p *histfixture.Pair, meta bool, db *int, n int,
	age time.Duration) {
	t.Helper()
	pool := p.Monitored
	if meta {
		pool = p.Meta
	}
	at := time.Now().Add(-age)
	sha := fmt.Sprintf("%064x", n)
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.workload_packs
		(database_id, window_from, window_to, format, content_sha256, tier, bytes,
		 built_at, built_by, manifest, body)
		VALUES ($1, $2, $3, 'pg_sage-workload/1', $4, 'local', 1, $2, 't', '{}', '\x00')`,
		db, at, at.Add(time.Hour), sha); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCleanerAgesOutEveryDatabasesWorkloadPacks(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	for i, db := range []int{histfixture.DatabaseID, histfixture.OtherDatabaseID} {
		insertPack(t, p, true, &db, 10*i+1, 181*24*time.Hour)
		insertPack(t, p, true, &db, 10*i+2, 179*24*time.Hour)
	}
	c := New(p.Meta, packCfg(), noopLog).ForHistoryStore()
	for i := 0; i < 3; i++ {
		c.RunOnce(ctx)
	}
	for _, db := range []int{histfixture.DatabaseID, histfixture.OtherDatabaseID} {
		if n := countHist(t, p, true, `SELECT count(*) FROM sage.workload_packs
			WHERE database_id = $1`, db); n != 1 {
			t.Fatalf("database %d keeps %d packs, want 1 (the 179-day-old one)", db, n)
		}
		if n := countHist(t, p, true, `SELECT count(*) FROM sage.workload_packs
			WHERE database_id = $1 AND built_at < now() - interval '180 days'`, db); n != 0 {
			t.Fatalf("database %d keeps %d expired packs", db, n)
		}
	}
	off := storeCfg() // workload_packs_days 0 keeps every pack
	insertPack(t, p, true, nil, 99, 400*24*time.Hour)
	New(p.Meta, off, noopLog).ForHistoryStore().RunOnce(ctx)
	if n := countHist(t, p, true, "SELECT count(*) FROM sage.workload_packs"); n != 3 {
		t.Fatalf("workload_packs_days 0 must keep every pack: %d left, want 3", n)
	}
}

func TestPerDatabaseCleanerInMetaModeAgesItsLeftoverPacks(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	insertPack(t, p, false, nil, 1, 181*24*time.Hour) // built before the switch
	insertPack(t, p, false, nil, 2, time.Hour)
	New(p.Monitored, packCfg(), noopLog).RunOnce(context.Background())
	if n := countHist(t, p, false, "SELECT count(*) FROM sage.workload_packs"); n != 1 {
		t.Fatalf("monitored database keeps %d packs, want 1 (the recent one)", n)
	}
}
