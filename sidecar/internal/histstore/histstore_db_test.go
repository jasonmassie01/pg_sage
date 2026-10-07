package histstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/histstore"))
}

// seedMonitored writes a realistic history into the monitored database:
// snapshots through the delta writer (keyframes, checkpoints and deltas
// over two UTC days) and query_store samples through the recorder.
func seedMonitored(t *testing.T, pool *pgxpool.Pool, start time.Time, cycles int) {
	t.Helper()
	ctx := context.Background()
	sc := snapfixture.Scenario{Start: start, Step: 30 * time.Minute, Cycles: cycles,
		Tables: 6, Indexes: 8, Seed: 42, Events: snapfixture.Events{QueryChurn: 5}}
	gen, err := sc.Generate()
	if err != nil {
		t.Fatalf("generate scenario: %v", err)
	}
	w := snapstore.NewWriter()
	for i, c := range gen {
		rows := make([]snapstore.Row, 0, len(c.Docs))
		for _, d := range c.Docs {
			rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
		}
		if err := w.Persist(ctx, pool, c.At, rows); err != nil {
			t.Fatalf("persist cycle %d: %v", i, err)
		}
		samples := []querystore.Sample{
			{QueryID: 101, Calls: int64(10 * (i + 1)), TotalExecMs: float64(25 * (i + 1)),
				MeanExecMs: 2.5, Rows: int64(i)},
			{QueryID: 202, Calls: int64(3 * (i + 1)), TotalExecMs: float64(90 * (i + 1)),
				MeanExecMs: 30, Rows: 1},
		}
		if err := querystore.Record(ctx, pool, samples); err != nil {
			t.Fatalf("record samples %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE sage.query_store SET captured_at = $1
			WHERE captured_at > $1`, c.At); err != nil {
			t.Fatalf("date samples %d: %v", i, err)
		}
	}
}

// decoded lists every snapshot of the database as the readers see it.
func decoded(t *testing.T, db *pgxpool.Pool, where string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT collected_at::text || ' ' ||
		category || ' ' || COALESCE(`+snapstore.DataSQL("")+`::text, 'UNREADABLE')
		FROM sage.snapshots `+where+` ORDER BY collected_at, category`, args...)
	if err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func samples(t *testing.T, db *pgxpool.Pool, where string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT concat_ws(' ', captured_at, queryid,
		calls, total_exec_time, mean_exec_time, rows, plan_hash, stats_epoch)
		FROM sage.query_store `+where+` ORDER BY captured_at, queryid`, args...)
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func equal(t *testing.T, what string, a, b []string) {
	t.Helper()
	if len(a) == 0 {
		t.Fatalf("%s: the fixture is empty: the comparison proves nothing", what)
	}
	if len(a) != len(b) {
		t.Fatalf("%s: %d rows vs %d rows", what, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s: row %d differs:\n%s\n%s", what, i, a[i], b[i])
		}
	}
}

func day(t *testing.T) time.Time {
	t.Helper()
	// Starts the evening before yesterday so the history spans a UTC midnight.
	return time.Now().UTC().Truncate(24 * time.Hour).Add(-28 * time.Hour)
}

func migrate(t *testing.T, src, dst histstore.Store,
	opt histstore.MigrateOptions) histstore.MigrateReport {
	t.Helper()
	rep, err := histstore.Migrate(context.Background(), src, dst, opt)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return rep
}

func table(t *testing.T, rep histstore.MigrateReport, name string) histstore.TableReport {
	t.Helper()
	for _, tr := range rep.Tables {
		if tr.Table == name {
			return tr
		}
	}
	t.Fatalf("report has no table %s: %+v", name, rep)
	return histstore.TableReport{}
}
