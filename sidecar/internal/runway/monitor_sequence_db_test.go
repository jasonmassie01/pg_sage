package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-05 end to end on real PostgreSQL: the runway is the one
// sequence forecast. A bigint sequence owned by an int4 column projects
// against 2147483647; a descending one against the column's minimum; a
// CYCLE descending one opens nothing. No forecast_sequence_exhaustion
// row is ever written.

// seedSeries writes 12 samples over the last hour of subject in the
// epoch "seed", rising by rate per second to just below value (the
// value the live sample continues from) toward limit.
func seedSeries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subject string,
	value, rate, limit float64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.runway_samples WHERE subject = $1", subject)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'sequence', $1, 'seed', now() - make_interval(mins => 60 - i * 5),
		       $2 - (12 - i) * 300 * $3, $2 - (12 - i) * 300 * $3, $4
		FROM generate_series(0, 11) i`, subject, value, rate, limit); err != nil {
		t.Fatalf("seed %s: %v", subject, err)
	}
}

func liveSequence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ddl string) {
	t.Helper()
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

func findingDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ident string) (int, map[string]any) {
	t.Helper()
	var n int
	var detail map[string]any
	rows, err := pool.Query(ctx, `SELECT category, detail FROM sage.findings
		WHERE object_identifier = $1 AND status = 'open'`, ident)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cat string
		var d map[string]any
		if err := rows.Scan(&cat, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if cat == CategorySequence {
			detail = d
		}
	}
	return n, detail
}

func TestMonitorTick_OneSequenceForecastAgainstTheColumnLimit(t *testing.T) {
	pool, ctx := livePool(t)
	id := time.Now().UnixNano()
	tbl, seq := fmt.Sprintf("mon_one_%d", id), fmt.Sprintf("mon_one_%d_seq", id)
	liveSequence(t, ctx, pool, fmt.Sprintf(`CREATE TABLE %[1]s (id int);
		CREATE SEQUENCE %[2]s OWNED BY %[1]s.id;
		SELECT setval('%[2]s', 2100000000);
		SELECT nextval('%[2]s') FROM generate_series(1, 5);`, tbl, seq))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+tbl) })
	name := "public." + seq
	seedSeries(t, ctx, pool, name, 2100000000, 55, 2147483647) // about 10 days left
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), &fakeStarter{})
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	n, detail := findingDetail(t, ctx, pool, name)
	if n != 1 || detail == nil || detail["limit"] != 2147483647.0 {
		t.Fatalf("%d open findings, runway detail %v: want exactly one "+
			"forecast_sequence_runway against 2147483647", n, detail)
	}
	var retired int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category = 'forecast_sequence_exhaustion'`).Scan(&retired); err != nil ||
		retired != 0 {
		t.Fatalf("forecast_sequence_exhaustion rows = %d (%v), want 0", retired, err)
	}
}

func TestMonitorTick_DescendingSequenceRunways(t *testing.T) {
	pool, ctx := livePool(t)
	id := time.Now().UnixNano()
	tbl := fmt.Sprintf("mon_down_%d", id)
	down, cyc := tbl+"_seq", tbl+"_cyc"
	liveSequence(t, ctx, pool, fmt.Sprintf(`CREATE TABLE %[1]s (id int, c int);
		CREATE SEQUENCE %[2]s INCREMENT -1 OWNED BY %[1]s.id;
		SELECT setval('%[2]s', -2100000000);
		SELECT nextval('%[2]s') FROM generate_series(1, 5);
		CREATE SEQUENCE %[3]s INCREMENT -1 MINVALUE -2147483648 MAXVALUE -1 CYCLE
		    OWNED BY %[1]s.c;
		SELECT setval('%[3]s', -2100000000);
		SELECT nextval('%[3]s') FROM generate_series(1, 5);`, tbl, down, cyc))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+tbl) })
	// Descending series are negated: 2100000000 rising toward 2147483648.
	seedSeries(t, ctx, pool, "public."+down, 2100000000, 55, 2147483648)
	seedSeries(t, ctx, pool, "public."+cyc, 2100000000, 55, 2147483648)
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), &fakeStarter{})
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	n, detail := findingDetail(t, ctx, pool, "public."+down)
	if n != 1 || detail == nil || detail["limit"] != -2147483648.0 ||
		detail["direction"] != "descending" {
		t.Fatalf("descending: %d findings, detail %v: want one runway at -2147483648", n,
			detail)
	}
	if n, d := findingDetail(t, ctx, pool, "public."+cyc); n != 0 {
		t.Fatalf("a CYCLE descending sequence opened %d findings (%v)", n, d)
	}
}
