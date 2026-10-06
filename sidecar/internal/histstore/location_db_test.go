package histstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

func metaPlacement(p *histfixture.Pair, t *testing.T) histstore.Placement {
	return histstore.Placement{Name: "app", Monitored: p.Monitored, Store: p.MetaStore(t),
		Meta: p.Meta, DatabaseID: histfixture.DatabaseID}
}

func monitoredPlacement(p *histfixture.Pair) histstore.Placement {
	return histstore.Placement{Name: "app", Monitored: p.Monitored,
		Store: histstore.NewMonitored(p.Monitored), Meta: p.Meta,
		DatabaseID: histfixture.DatabaseID}
}

func requireRefused(t *testing.T, err error, wants ...string) {
	t.Helper()
	if !errors.Is(err, histstore.ErrMigrationNeeded) {
		t.Fatalf("want ErrMigrationNeeded, got %v", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("refusal must say %q:\n%v", w, err)
		}
	}
}

func TestLocationFreshDatabaseStartsInEitherMode(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	p.Noise(t, time.Now(), 1) // another database's history is no reason to refuse
	ctx := context.Background()
	if err := histstore.CheckLocation(ctx, metaPlacement(p, t)); err != nil {
		t.Fatalf("meta mode, no history anywhere: %v", err)
	}
	if err := histstore.CheckLocation(ctx, monitoredPlacement(p)); err != nil {
		t.Fatalf("monitored mode, no history anywhere: %v", err)
	}
}

func TestLocationMetaRefusesUnmigratedHistory(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 3)
	ctx := context.Background()
	err := histstore.CheckLocation(ctx, metaPlacement(p, t))
	requireRefused(t, err, `"app"`, "history migrate", "--to meta")

	migrate(t, histstore.NewMonitored(p.Monitored), p.MetaStore(t), histstore.MigrateOptions{})
	if err := histstore.CheckLocation(ctx, metaPlacement(p, t)); err != nil {
		t.Fatalf("after a complete migration meta mode must start: %v", err)
	}
	// A sidecar still in monitored mode wrote another cycle after the copy.
	if _, err := p.Monitored.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category,
		data) VALUES (now(), 'system', '{}')`); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, histstore.CheckLocation(ctx, metaPlacement(p, t)), "history migrate")
}

func TestLocationMetaRefusesUnmigratedQueryStoreAlone(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	if _, err := p.Monitored.Exec(context.Background(), `INSERT INTO sage.query_store
		(queryid, calls, total_exec_time, mean_exec_time) VALUES (1, 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, histstore.CheckLocation(context.Background(), metaPlacement(p, t)),
		"query_store")
}

func TestLocationMetaStartsAfterCleanup(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 3)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	migrate(t, src, dst, histstore.MigrateOptions{})
	if _, err := histstore.Cleanup(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if err := histstore.CheckLocation(context.Background(), metaPlacement(p, t)); err != nil {
		t.Fatalf("after cleanup: %v", err)
	}
}

func TestLocationMonitoredRefusesHistoryLeftInTheStore(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	if _, err := p.Meta.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
		database_id) VALUES (now(), 'system', '{}', 7)`); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, histstore.CheckLocation(ctx, monitoredPlacement(p)),
		"history migrate", "--to monitored")
	migrate(t, p.MetaStore(t), histstore.NewMonitored(p.Monitored), histstore.MigrateOptions{})
	if err := histstore.CheckLocation(ctx, monitoredPlacement(p)); err != nil {
		t.Fatalf("after copying back monitored mode must start: %v", err)
	}
}

func TestLocationMonitoredWithoutStoreSchemaOrMeta(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	standalone := histstore.Placement{Name: "app", Monitored: p.Monitored,
		Store: histstore.NewMonitored(p.Monitored)}
	if err := histstore.CheckLocation(ctx, standalone); err != nil {
		t.Fatalf("standalone (no meta database): %v", err)
	}
	// A meta database that was never a history store has no database_id
	// column: nothing to look for there. The monitored database stands in.
	plain := histstore.Placement{Name: "app", Monitored: p.Meta,
		Store: histstore.NewMonitored(p.Meta), Meta: p.Monitored, DatabaseID: 7}
	if err := histstore.CheckLocation(ctx, plain); err != nil {
		t.Fatalf("meta database without the store schema: %v", err)
	}
}

func TestLocationRejectsIncompletePlacement(t *testing.T) {
	ctx := context.Background()
	if err := histstore.CheckLocation(ctx, histstore.Placement{Name: "app"}); !errors.Is(err,
		histstore.ErrNoDatabase) {
		t.Fatalf("no monitored database: want ErrNoDatabase, got %v", err)
	}
}

func TestOpenMonitoredScopesTheMetaDatabasesOwnRows(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	ctx := context.Background()
	// The meta database monitored as itself in monitored mode: its own rows
	// have no database_id; the store's rows of database 8 must stay out.
	p.Noise(t, time.Now(), 5)
	if _, err := p.Meta.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now(), 'system', '{"own":true}')`); err != nil {
		t.Fatal(err)
	}
	s, err := histstore.OpenMonitored(ctx, p.Meta)
	if err != nil {
		t.Fatalf("open monitored: %v", err)
	}
	var n int
	if err := s.QueryRow(ctx, `SELECT count(*) FROM sage.snapshots s WHERE {db:s}`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("monitored store on the meta database read %d rows, want only its own 1", n)
	}
	plain, err := histstore.OpenMonitored(ctx, p.Monitored)
	if err != nil || plain.Scoped() {
		t.Fatalf("a plain monitored database: %+v %v", plain, err)
	}
}

func TestStoreDatabasesRegistry(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	meta := p.MetaStore(t)
	if err := histstore.RegisterDatabase(ctx, meta, "app"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := histstore.UpdateDatabaseSize(ctx, meta, 5<<30); err != nil {
		t.Fatalf("size: %v", err)
	}
	other, _ := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err := histstore.RegisterDatabase(ctx, other, "other"); err != nil {
		t.Fatal(err)
	}
	dbs, err := histstore.StoreDatabases(ctx, p.Meta, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dbs) != 2 || dbs[0].ID != 7 || dbs[0].Name != "app" || dbs[0].DBBytes != 5<<30 {
		t.Fatalf("registry: %+v", dbs)
	}
	if err := histstore.RegisterDatabase(ctx, histstore.NewMonitored(p.Monitored),
		"x"); err == nil {
		t.Fatal("a monitored store has no registry row to write")
	}
}

func TestHistoryBytesIsThisDatabasesShare(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	meta := p.MetaStore(t)
	if b, err := histstore.NewMonitored(p.Monitored).HistoryBytes(ctx); err != nil || b != 0 {
		t.Fatalf("monitored HistoryBytes: %d %v (its history is in the schema size)", b, err)
	}
	empty, err := meta.HistoryBytes(ctx)
	if err != nil {
		t.Fatalf("history bytes: %v", err)
	}
	seedMonitored(t, p.Monitored, day(t), 6) // through Resolve: lands in the store
	p.Noise(t, time.Now(), 1, 2, 3)
	if _, err := p.Meta.Exec(ctx, "ANALYZE sage.snapshots, sage.query_store"); err != nil {
		t.Fatal(err)
	}
	full, err := meta.HistoryBytes(ctx)
	if err != nil {
		t.Fatalf("history bytes: %v", err)
	}
	if full <= empty || full <= 0 {
		t.Fatalf("history bytes must grow with the database's rows: %d -> %d", empty, full)
	}
	var total int64
	if err := p.Meta.QueryRow(ctx, `SELECT
		(SELECT sum(pg_total_relation_size(relid)) FROM pg_partition_tree('sage.snapshots'))
		+ (SELECT sum(pg_total_relation_size(relid))
		   FROM pg_partition_tree('sage.query_store'))`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if full >= total {
		t.Fatalf("this database's share (%d) must be less than the store's history (%d): "+
			"another database's rows are in it", full, total)
	}
}
