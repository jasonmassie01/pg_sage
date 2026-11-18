package schemadebt

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/schemadebt"))
}

const (
	liveOrdersSeq  = "review02d.orders_id_seq"
	liveRefundsSeq = "review02d.refunds_id_seq"
)

// livePool bootstraps the sage schema twice (the second run must be a
// clean no-op) and builds review02d.orders (a serial int4 key) and
// review02d.refunds (an int4 key fed by a descending sequence).
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", i+1, err)
		}
	}
	for _, sql := range []string{"DROP SCHEMA IF EXISTS review02d CASCADE",
		"DELETE FROM sage.runway_samples WHERE subject LIKE 'review02d.%'",
		"CREATE SCHEMA review02d",
		"CREATE TABLE review02d.orders (id serial PRIMARY KEY, " +
			"placed_at timestamp without time zone, note text)",
		"CREATE TABLE review02d.refunds (id integer PRIMARY KEY)",
		"CREATE SEQUENCE review02d.refunds_id_seq AS integer INCREMENT BY -1 " +
			"MINVALUE -2147483648 MAXVALUE -1 START WITH -1 OWNED BY review02d.refunds.id",
		"ALTER TABLE review02d.refunds ALTER COLUMN id " +
			"SET DEFAULT nextval('review02d.refunds_id_seq')",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return pool, ctx
}

// recordSamples advances both sequences by 1e6 a sample with setval, has
// the runway monitor record each sample, then spreads the samples one day
// apart ending now, as five daily runway samples would be.
func recordSamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	m, err := runway.NewMonitor(pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), nil, runway.Options{Database: "db1", Interval: time.Minute,
		Lookback: 24 * time.Hour, MinSamples: 3, Retention: 30 * 24 * time.Hour,
		SequenceHorizon: 24 * time.Hour, SequenceCritical: time.Hour}, nil)
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	for i := 0; i < 5; i++ {
		v := 1.896e9 + float64(i)*1e6
		if _, err := pool.Exec(ctx, `SELECT setval('review02d.orders_id_seq', $1::bigint),
			setval('review02d.refunds_id_seq', -$1::bigint)`, int64(v)); err != nil {
			t.Fatalf("setval: %v", err)
		}
		if _, err := m.Sample(ctx); err != nil {
			t.Logf("sample %d reported: %v", i+1, err) // other series may not read
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.runway_samples
		WHERE kind = 'sequence' AND subject IN ($1, $2)`, liveOrdersSeq,
		liveRefundsSeq).Scan(&n); err != nil || n != 10 {
		t.Fatalf("the monitor recorded %d sequence samples (%v), want 10", n, err)
	}
	if _, err := pool.Exec(ctx, `WITH r AS (
		SELECT id, row_number() OVER (PARTITION BY subject ORDER BY id DESC) - 1 AS k
		  FROM sage.runway_samples WHERE kind = 'sequence' AND subject LIKE 'review02d.%')
		UPDATE sage.runway_samples s SET sampled_at = now() - make_interval(days => r.k::int)
		  FROM r WHERE s.id = r.id`); err != nil {
		t.Fatalf("spread the samples: %v", err)
	}
	// an older epoch of the same sequence is not this series
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		VALUES ('sequence', $1, 'older', now() - interval '20 days', 5, 5, 2147483647)`,
		liveOrdersSeq); err != nil {
		t.Fatalf("seed an older epoch: %v", err)
	}
}

// livePack states the fixture tables as the pack's schema section does.
func livePack(t *testing.T) *workpack.Pack {
	t.Helper()
	now := time.Now().UTC()
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1",
		Window: workpack.Window{From: now.AddDate(0, 0, -90), To: now},
		Schema: &workpack.Schema{TablesTotal: 2, Tables: []workpack.TableFact{
			{ID: "schema/table/review02d.orders", Table: "review02d.orders", Kind: "table",
				Columns: []workpack.ColumnFact{{Name: "id", Type: "integer", NotNull: true},
					{Name: "placed_at", Type: "timestamp without time zone"},
					{Name: "note", Type: "text"}},
				Constraints: []string{"orders_pkey PRIMARY KEY (id)"}},
			{ID: "schema/table/review02d.refunds", Table: "review02d.refunds", Kind: "table",
				Columns:     []workpack.ColumnFact{{Name: "id", Type: "integer", NotNull: true}},
				Constraints: []string{"refunds_pkey PRIMARY KEY (id)"}}}}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

func byTarget(t *testing.T, cands []candidate.Candidate, kind, target string,
) candidate.Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Kind == kind && c.Target == target {
			return c
		}
	}
	t.Fatalf("no %s candidate on %s in %+v", kind, target, cands)
	return candidate.Candidate{}
}

func TestDB_RealSequencesAndRunwaySamplesGiveAMeasuredRunway(t *testing.T) {
	pool, ctx := livePool(t)
	recordSamples(t, ctx, pool)
	src := NewDBSource(pool, catalogread.New(pool, catalogread.Default()))
	cands, cov, err := New(src, Options{}).Generate(ctx, livePack(t),
		candidate.Env{Database: "db1", WindowDays: 90})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ord := byTarget(t, cands, KindInt4Key, "review02d.orders.id")
	if ord.ActionType != capacityAction {
		t.Fatalf("orders key %s (%s), want %s; coverage %+v", ord.ActionType, ord.Reason,
			capacityAction, cov)
	}
	mustContain(t, "orders reason", ord.Reason, "exhausts in 247 days", "maximum 2147483647")
	mustContain(t, "orders source", ord.Prediction.Source, liveOrdersSeq, "5 samples")
	if ord.Prediction.Quantities[QuantitySamples] != 5 {
		t.Fatalf("the older epoch was counted: %v", ord.Prediction.Quantities)
	}
	mustContain(t, "orders script", ord.ForwardSQL, `"review02d"."orders_id_seq"`)
	swap := byTarget(t, cands, KindColumnSwap, "review02d.orders.id")
	mustContain(t, "swap", swap.ForwardSQL, "review02d.orders.id: integer -> bigint")
	ref := byTarget(t, cands, KindInt4Key, "review02d.refunds.id")
	mustContain(t, "refunds reason", ref.Reason, "exhausts in 247 days",
		"minimum -2147483648")
}

func TestDB_ColumnGoneSinceThePackIsAReadErrorNotAFailure(t *testing.T) {
	pool, ctx := livePool(t)
	src := NewDBSource(pool, catalogread.New(pool, catalogread.Default()))
	gone := ColumnRef{Schema: "review02d", Table: "orders", Column: "nope"}
	in, err := src.Read(ctx, []ColumnRef{gone,
		{Schema: "review02d", Table: "orders", Column: "id"}}, time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if in.Columns[gone].ReadErr == "" {
		t.Fatalf("a missing column read %+v, want a read error", in.Columns[gone])
	}
	id := in.Columns[ColumnRef{Schema: "review02d", Table: "orders", Column: "id"}]
	if id.Sequence != liveOrdersSeq || id.Increment != 1 || id.Facts.Column.Type != "integer" {
		t.Fatalf("orders.id read %+v", id)
	}
	if len(in.Series) != 0 {
		t.Fatalf("series %+v before any sample was recorded", in.Series)
	}
}

func TestDB_SourceCapsTheColumnsItReads(t *testing.T) {
	pool, ctx := livePool(t)
	src := NewDBSource(pool, catalogread.New(pool, catalogread.Default()))
	cols := make([]ColumnRef, MaxColumns+5)
	for i := range cols {
		cols[i] = ColumnRef{Schema: "review02d", Table: "orders", Column: "id"}
	}
	cols[len(cols)-1].Column = "placed_at"
	in, err := src.Read(ctx, cols, time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !in.Truncated {
		t.Fatalf("reading %d columns was not marked truncated", len(cols))
	}
}
