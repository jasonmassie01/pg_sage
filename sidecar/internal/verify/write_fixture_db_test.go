package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// writeFixture runs a real workload on one dedicated connection and stores
// collector cycles (system and tables rows, through snapstore as the
// collector does) at chosen collection times. Every statement that writes
// tuples goes through write, so a cycle can wait until the statistics
// system shows them: PG 14's collector and PG 15+'s shared statistics both
// publish a backend's counts some time after its transaction.
type writeFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	conn     *pgxpool.Conn
	writer   *snapstore.Writer
	at       time.Time
	tables   []string
	expected map[string]int64
	epoch    string
}

func newWriteFixture(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time, tables ...string,
) *writeFixture {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire fixture connection: %v", err)
	}
	f := &writeFixture{t: t, ctx: ctx, pool: pool, conn: conn, writer: snapstore.NewWriter(),
		at: at, tables: tables, expected: map[string]int64{}}
	f.dropTables()
	f.cleanup()
	t.Cleanup(func() {
		conn.Release()
		f.dropTables()
		f.cleanup()
	})
	var epoch time.Time
	if err := conn.QueryRow(ctx, `SELECT GREATEST(stats_reset, pg_postmaster_start_time())
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&epoch); err != nil {
		t.Fatalf("read statistics epoch: %v", err)
	}
	raw, _ := json.Marshal(epoch.UTC())
	f.epoch = string(raw[1 : len(raw)-1])
	return f
}

func (f *writeFixture) dropTables() {
	for _, table := range f.tables {
		_, _ = f.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	}
}

// cleanup removes the fixture's cycles and query samples.
func (f *writeFixture) cleanup() {
	ctx := context.Background()
	_, _ = f.pool.Exec(ctx, `DELETE FROM sage.snapshots WHERE category IN ('system', 'tables')
		AND collected_at BETWEEN $1 AND $2`, f.at.Add(-3*time.Hour), f.at.Add(3*time.Hour))
	_, _ = f.pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1",
		writeSignalQueryID)
}

func (f *writeFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.conn.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture SQL failed: %v\n%s", err, sql)
	}
}

// write runs a statement that writes tuples tuples to table.
func (f *writeFixture) write(table string, tuples int64, sql string, args ...any) {
	f.t.Helper()
	f.exec(sql, args...)
	f.expected[table] += tuples
}

// settle waits until the statistics show every tuple written so far. Each
// poll ends a transaction on the writing backend, which is when it reports.
func (f *writeFixture) settle() {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for table, want := range f.expected {
		for {
			var got int64
			err := f.conn.QueryRow(f.ctx, `SELECT COALESCE(sum(n_tup_ins + n_tup_upd +
				n_tup_del), 0)::int8 FROM pg_stat_user_tables
				WHERE schemaname = 'public' AND relname = $1`, table).Scan(&got)
			if err != nil {
				f.t.Fatalf("read %s counters: %v", table, err)
			}
			if got == want {
				break
			}
			if time.Now().After(deadline) {
				f.t.Fatalf("%s statistics show %d written tuples, want %d", table, got, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// cycle stores the server's real WAL position and table counters as the
// collector cycle collected at at.
func (f *writeFixture) cycle(at time.Time) {
	f.t.Helper()
	f.settle()
	var lsn int64
	var tables []byte
	err := f.conn.QueryRow(f.ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::int8,
		COALESCE((SELECT jsonb_agg(jsonb_build_object('schemaname', schemaname,
			'relname', relname, 'n_tup_ins', n_tup_ins, 'n_tup_upd', n_tup_upd,
			'n_tup_del', n_tup_del) ORDER BY relid)
		FROM pg_stat_user_tables WHERE schemaname <> 'sage'), '[]'::jsonb)`).
		Scan(&lsn, &tables)
	if err != nil {
		f.t.Fatalf("read cycle counters: %v", err)
	}
	f.persist(at, f.systemDoc(lsn, 0), tables)
}

func (f *writeFixture) systemDoc(lsn int64, blkWriteTime float64) []byte {
	doc, _ := json.Marshal(map[string]any{"wal_lsn": lsn, "relation_stats_epoch": f.epoch,
		"is_replica": false, "blk_write_time": blkWriteTime})
	return doc
}

// systemOnly stores a cycle whose tables category was not collected.
func (f *writeFixture) systemOnly(at time.Time, lsn int64, blkWriteTime float64) {
	f.t.Helper()
	f.persist(at, f.systemDoc(lsn, blkWriteTime), nil)
}

// synthetic stores a cycle at which the fixture's first table had written
// tuples tuples, beside an idle table whose counters never move.
func (f *writeFixture) synthetic(at time.Time, lsn, tuples int64) {
	f.t.Helper()
	tables := fmt.Sprintf(`[{"schemaname":"public","relname":"verify_wsig_idle",
		"n_tup_ins":7,"n_tup_upd":0,"n_tup_del":0},{"schemaname":"public","relname":%q,
		"n_tup_ins":%d,"n_tup_upd":0,"n_tup_del":0}]`, f.tables[0], tuples)
	f.persist(at, f.systemDoc(lsn, 0), []byte(tables))
}

func (f *writeFixture) persist(at time.Time, system, tables []byte) {
	f.t.Helper()
	rows := []snapstore.Row{{Category: "system", Data: system}}
	if tables != nil {
		rows = append(rows, snapstore.Row{Category: "tables", Data: tables})
	}
	if err := f.writer.Persist(f.ctx, f.pool, at, rows); err != nil {
		f.t.Fatalf("persist cycle at %s: %v", at, err)
	}
}
