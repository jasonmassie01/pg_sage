package rubric

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// T6 round 1 (review-01a): the grade reads whether the analyzer ran from
// the database's history store. In meta mode the monitored database's own
// sage.snapshots is not its history: the grade must read the meta store,
// scoped to this database's id. No concurrency test: producersRan holds
// no state between calls.
func TestLoaderDB_ProducersReadTheMetaHistoryOfThisDatabaseOnly(t *testing.T) {
	monitored, ctx := livePool(t)
	meta, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rubricmeta"))
	if err != nil {
		t.Fatalf("connect the meta database: %v", err)
	}
	t.Cleanup(meta.Close)
	if err := schema.Bootstrap(ctx, meta); err != nil {
		t.Fatalf("bootstrap the meta database: %v", err)
	}
	if err := schema.BootstrapHistoryStore(ctx, meta); err != nil {
		t.Fatalf("make the meta database a history store: %v", err)
	}
	// The monitored database's own table has a snapshot (an old copy, or
	// rows written before the move to meta): it must not count.
	execAll(t, ctx, monitored, "DELETE FROM sage.snapshots",
		`INSERT INTO sage.snapshots (collected_at, category, data)
		 VALUES (now(), 'system', '{}'::jsonb)`)
	// Another database's history in the shared meta store must not count.
	execAll(t, ctx, meta, `INSERT INTO sage.snapshots (collected_at, category, data,
		database_id) VALUES (now(), 'system', '{}'::jsonb, 8)`)
	store, err := histstore.NewMeta(meta, 7)
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	t.Cleanup(histstore.Register(monitored, "orders", store))
	l := &DBLoader{Pool: monitored, Database: "orders"}
	ran, err := l.producersRan(ctx)
	if err != nil {
		t.Fatalf("producers: %v", err)
	}
	if ran["analyzer"] || ran["migration"] || ran["forecaster"] {
		t.Fatalf("ran = %v: read history that is not this database's", ran)
	}
	execAll(t, ctx, meta, `INSERT INTO sage.snapshots (collected_at, category, data,
		database_id) VALUES (now(), 'system', '{}'::jsonb, 7)`)
	ran, err = l.producersRan(ctx)
	if err != nil {
		t.Fatalf("producers after a meta snapshot: %v", err)
	}
	if !ran["analyzer"] || !ran["migration"] || !ran["upgradecheck"] || ran["forecaster"] {
		t.Fatalf("ran = %v, want the analyzer seen from this database's meta snapshot "+
			"and no day-old history yet", ran)
	}
}

// Error propagation: a meta store that cannot be read fails the read with
// the database named, instead of reporting that nothing ran.
func TestLoaderDB_UnreadableMetaHistoryIsAnError(t *testing.T) {
	monitored, ctx := livePool(t)
	meta, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rubricmetadown"))
	if err != nil {
		t.Fatalf("connect the meta database: %v", err)
	}
	store, err := histstore.NewMeta(meta, 7)
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	t.Cleanup(histstore.Register(monitored, "orders", store))
	meta.Close()
	ran, err := (&DBLoader{Pool: monitored, Database: "orders"}).producersRan(ctx)
	if err == nil || ran != nil {
		t.Fatalf("producersRan = %v, %v; want an error", ran, err)
	}
	if got := err.Error(); !strings.Contains(got, "snapshot history of orders") {
		t.Fatalf("error %q does not name the history read and the database", got)
	}
}
