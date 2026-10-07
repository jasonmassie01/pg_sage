// Package histfixture runs a history reader once with pg_sage's history in
// the monitored database and once with it in the meta database
// (history.store: meta), on the same fixture, so tests can require the
// two answers to be identical. The meta database also holds another
// database's history (Noise) with colliding query ids and larger numbers:
// a reader that forgets to scope its rows to its own database answers
// differently in meta mode, and the comparison fails.
package histfixture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// DatabaseID is the meta-db record id of the monitored database under test.
const DatabaseID = 7

// OtherDatabaseID is another monitored database whose history shares the
// meta database's tables.
const OtherDatabaseID = 8

// Pair is a monitored database and a meta database with the history store
// bootstrapped.
type Pair struct {
	Monitored *pgxpool.Pool
	Meta      *pgxpool.Pool
	unreg     func()
}

// NewPair creates both databases (skipping without a live test server) and
// bootstraps the sage schema in each and the history store in the meta one.
func NewPair(t testing.TB) *Pair {
	t.Helper()
	ctx := context.Background()
	p := &Pair{
		Monitored: connect(t, testdb.CreateDatabase(t, "hist_monitored")),
		Meta:      connect(t, testdb.CreateDatabase(t, "hist_meta")),
	}
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap sage schema: %v", err)
		}
	}
	if err := schema.BootstrapHistoryStore(ctx, p.Meta); err != nil {
		t.Fatalf("bootstrap history store: %v", err)
	}
	t.Cleanup(func() { p.unregister() })
	return p
}

func connect(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Modes are both placements, monitored first.
func Modes() []histstore.Mode {
	return []histstore.Mode{histstore.ModeMonitored, histstore.ModeMeta}
}

// MetaStore is the history store of the monitored database in the meta
// database.
func (p *Pair) MetaStore(t testing.TB) histstore.Store {
	t.Helper()
	s, err := histstore.NewMeta(p.Meta, DatabaseID)
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	return s
}

// Switch empties the history of both databases and places the monitored
// database's history in mode: registered in the meta database, or not
// registered (the monitored database itself).
func (p *Pair) Switch(t testing.TB, mode histstore.Mode) {
	t.Helper()
	p.unregister()
	p.Truncate(t)
	if mode == histstore.ModeMeta {
		p.unreg = histstore.Register(p.Monitored, "monitored", p.MetaStore(t))
	}
}

func (p *Pair) unregister() {
	if p.unreg != nil {
		p.unreg()
		p.unreg = nil
	}
}

// Truncate empties sage.snapshots and sage.query_store in both databases.
func (p *Pair) Truncate(t testing.TB) {
	t.Helper()
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(),
			"TRUNCATE sage.snapshots, sage.query_store"); err != nil {
			t.Fatalf("truncate history: %v", err)
		}
	}
}

// Noise writes another database's history into the meta database at at:
// a system, queries and sequences snapshot and query_store samples for
// qids, all with numbers far from any fixture's.
func (p *Pair) Noise(t testing.TB, at time.Time, qids ...int64) {
	t.Helper()
	ctx := context.Background()
	queries := "["
	for i, q := range qids {
		if i > 0 {
			queries += ","
		}
		queries += fmt.Sprintf(`{"queryid":%d,"calls":900000000,"total_exec_time":`+
			`9.0e12,"mean_exec_time":10000,"rows":1}`, q)
	}
	queries += "]"
	docs := map[string]string{
		"system": `{"db_size_bytes":999000000000,"active_backends":999,` +
			`"total_backends":999,"max_connections":9999,"cache_hit_ratio":0.01,` +
			`"total_checkpoints":999999,"blk_write_time":9999999}`,
		"queries": queries,
		"sequences": `[{"schemaname":"public","sequencename":"noise_seq",` +
			`"pct_used":99,"max_value":100}]`,
	}
	for cat, doc := range docs {
		if _, err := p.Meta.Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data, database_id) VALUES ($1, $2, $3, $4)`,
			at, cat, doc, OtherDatabaseID); err != nil {
			t.Fatalf("noise snapshot %s: %v", cat, err)
		}
	}
	for _, q := range qids {
		if _, err := p.Meta.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid,
			calls, total_exec_time, mean_exec_time, rows, plan_hash, database_id)
			VALUES ($1, $2, 900000000, 9.0e12, 10000, 1, 'noise', $3)`,
			at, q, OtherDatabaseID); err != nil {
			t.Fatalf("noise query_store %d: %v", q, err)
		}
	}
}

// Count returns how many rows of table (snapshots or query_store) the
// monitored and the meta database hold for the monitored database.
func (p *Pair) Count(t testing.TB, table string) (monitored, meta int64) {
	t.Helper()
	ctx := context.Background()
	if table != "snapshots" && table != "query_store" {
		t.Fatalf("count: unknown history table %q", table)
	}
	if err := p.Monitored.QueryRow(ctx, "SELECT count(*) FROM sage."+table).
		Scan(&monitored); err != nil {
		t.Fatalf("count monitored %s: %v", table, err)
	}
	if err := p.Meta.QueryRow(ctx, "SELECT count(*) FROM sage."+table+
		" WHERE database_id = $1", DatabaseID).Scan(&meta); err != nil {
		t.Fatalf("count meta %s: %v", table, err)
	}
	return monitored, meta
}
