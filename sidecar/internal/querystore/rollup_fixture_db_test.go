package querystore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// Fixture helpers of the rollup tests (evid-01a): snapshots are written
// through the real snapstore.Writer, so the rollup reads delta rows as a
// sidecar writes them.

// qc is one queryid's counters in one queries snapshot.
type qc struct {
	id, calls          int64
	exec               float64
	rows, hit, read    int64
	tread, twrite, wal int64
	omitWAL            bool
}

func (q qc) doc() map[string]any {
	m := map[string]any{"queryid": q.id, "query": "SELECT fixture", "calls": q.calls,
		"total_exec_time": q.exec, "mean_exec_time": 1.0, "rows": q.rows,
		"shared_blks_hit": q.hit, "shared_blks_read": q.read, "shared_blks_dirtied": 0,
		"shared_blks_written": 0, "temp_blks_read": q.tread, "temp_blks_written": q.twrite,
		"blk_read_time": 0.0, "blk_write_time": 0.0}
	if !q.omitWAL {
		m["wal_bytes"] = q.wal
	}
	return m
}

// sys is one system snapshot's statement fields; a nil epoch is a legacy
// row (written before evid-01a).
type sys struct {
	epoch   *time.Time
	exec    float64
	calls   int64
	dealloc int64
}

func (s sys) doc() map[string]any {
	m := map[string]any{"active_backends": 1, "total_backends": 2,
		"stat_statements_usage": map[string]any{"dealloc": s.dealloc}}
	if s.epoch != nil {
		m["statements_epoch"] = s.epoch.UTC().Format(time.RFC3339Nano)
		m["statements_total_exec_ms"] = s.exec
		m["statements_calls"] = s.calls
		m["statements_entries"] = 20
	}
	return m
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// persistCycle writes one cycle's queries and system snapshots.
func persistCycle(t testing.TB, w *snapstore.Writer, db snapstore.Beginner, at time.Time,
	qs []qc, s sys) {
	t.Helper()
	docs := make([]map[string]any, len(qs))
	for i, q := range qs {
		docs[i] = q.doc()
	}
	err := w.Persist(context.Background(), db, at, []snapstore.Row{
		{Category: "queries", Data: mustJSON(t, docs)},
		{Category: "system", Data: mustJSON(t, s.doc())},
	})
	if err != nil {
		t.Fatalf("persist cycle at %v: %v", at, err)
	}
}

// hourRow is one sage.query_rollup_hourly row.
type hourRow struct {
	calls                               int64
	exec                                float64
	rows, hit, read, tread, twrite, wal int64
	brk                                 bool
	pairs                               int
	planHash                            *string
}

func readHourRows(t testing.TB, db any, hour time.Time) map[int64]hourRow {
	t.Helper()
	rows, err := histstore.Resolve(db).Query(context.Background(), `SELECT r.queryid,
		r.calls, r.exec_ms, r.rows, r.shared_blks_hit, r.shared_blks_read, r.temp_blks_read,
		r.temp_blks_written, r.wal_bytes, r.epoch_break, r.pairs, r.plan_hash
		FROM sage.query_rollup_hourly r WHERE {db:r} AND r.hour = $1`, hour)
	if err != nil {
		t.Fatalf("read hourly rows: %v", err)
	}
	defer rows.Close()
	out := map[int64]hourRow{}
	for rows.Next() {
		var id int64
		var r hourRow
		var pairs int16
		if err := rows.Scan(&id, &r.calls, &r.exec, &r.rows, &r.hit, &r.read, &r.tread,
			&r.twrite, &r.wal, &r.brk, &pairs, &r.planHash); err != nil {
			t.Fatalf("scan hourly row: %v", err)
		}
		r.pairs = int(pairs)
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read hourly rows: %v", err)
	}
	return out
}

// coverage is one sage.query_rollup_coverage row.
type coverage struct {
	dbExec           *float64
	sampledExec      float64
	queryids, breaks int
	dealloc          *int64
	snapshots        int
	source           string
}

func readCoverage(t testing.TB, db any, hour time.Time) (coverage, bool) {
	t.Helper()
	var c coverage
	var q, b int32
	var n int16
	err := histstore.Resolve(db).QueryRow(context.Background(), `SELECT c.db_exec_ms,
		c.sampled_exec_ms, c.sampled_queryids, c.epoch_breaks, c.dealloc_delta, c.snapshots,
		c.epoch_source FROM sage.query_rollup_coverage c WHERE {db:c} AND c.hour = $1`,
		hour).Scan(&c.dbExec, &c.sampledExec, &q, &b, &c.dealloc, &n, &c.source)
	if err != nil {
		return coverage{}, false
	}
	c.queryids, c.breaks, c.snapshots = int(q), int(b), int(n)
	return c, true
}

// truncateRollup empties the rollup tables of both databases (Switch
// empties only the snapshots and the query store).
func truncateRollup(t testing.TB, p *histfixture.Pair) {
	t.Helper()
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(),
			"TRUNCATE sage.query_rollup_hourly, sage.query_rollup_coverage"); err != nil {
			t.Fatalf("truncate rollup: %v", err)
		}
	}
}

// pastHour is a closed UTC hour n hours before the current one.
func pastHour(n int) time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(n) * time.Hour)
}

// fixtureCounters is queryid id's counters at cycle i: every counter grows
// with i, at its own rate per queryid.
func fixtureCounters(id int64, i int) qc {
	n := int64(i + 1)
	return qc{id: id, calls: id * 10 * n, exec: float64(id)*1.25*float64(n) + 0.001*float64(i),
		rows: id * 3 * n, hit: id * 7 * n, read: id * n, tread: n, twrite: 2 * n,
		wal: id * 100 * n}
}

// sub is a minus b, field by field (the expected rollup of a window).
func sub(a, b qc) hourRow {
	return hourRow{calls: a.calls - b.calls, exec: a.exec - b.exec, rows: a.rows - b.rows,
		hit: a.hit - b.hit, read: a.read - b.read, tread: a.tread - b.tread,
		twrite: a.twrite - b.twrite, wal: a.wal - b.wal}
}
