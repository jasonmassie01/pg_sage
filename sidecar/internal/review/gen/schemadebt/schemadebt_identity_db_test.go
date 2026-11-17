package schemadebt

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

const liveEventsSeq = "review02d.events_id_seq"

// An identity int4 key is fed by its identity sequence (pg_depend deptype
// i): it gets a runway from the samples, read newest first and fitted
// oldest first, and the planner's identity refusal is attached.
func TestDB_IdentityKeyGetsARunwayAndThePlannerRefusal(t *testing.T) {
	pool, ctx := livePool(t)
	seedIdentitySamples(t, ctx, pool)
	src := NewDBSource(pool, catalogread.New(pool, catalogread.Default()))
	events := ColumnRef{Schema: "review02d", Table: "events", Column: "id"}
	in, err := src.Read(ctx, []ColumnRef{events}, time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	col := in.Columns[events]
	if col.Sequence != liveEventsSeq || col.Increment != 1 || !col.Facts.Column.Identity {
		t.Fatalf("events.id read %+v, want identity fed by %s", col, liveEventsSeq)
	}
	pts := in.Series[liveEventsSeq].Points
	if len(pts) != 40 || !pts[0].At.Before(pts[39].At) || pts[39].Value != 1.9e9 {
		t.Fatalf("series of %d points, first %+v last %+v: want 40 oldest first", len(pts),
			pts[0], pts[len(pts)-1])
	}
	pack := identityPack(t)
	cands, _, err := New(src, Options{}).Generate(ctx, pack,
		candidate.Env{Database: "db1", WindowDays: 90})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := byTarget(t, cands, KindInt4Key, "review02d.events.id")
	if c.ActionType != capacityAction || c.Prediction.Quantities[QuantitySamples] != 40 {
		t.Fatalf("identity key %s with %v", c.ActionType, c.Prediction.Quantities)
	}
	mustContain(t, "reason", c.Reason, "exhausts in 247 days", "online column swap refused",
		"identity")
	mustContain(t, "script", c.ForwardSQL, `"review02d"."events_id_seq"`)
	if n := len(ofKind(cands, KindColumnSwap)); n != 0 {
		t.Fatalf("%d column swaps for an identity key the planner refuses", n)
	}
}

// seedIdentitySamples builds review02d.events (an identity int4 key) and
// records 40 daily samples of its sequence ending now, newest first,
// rising 1e6 a day to 1.9e9.
func seedIdentitySamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, sql := range []string{
		"CREATE TABLE review02d.events (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY)",
		"DELETE FROM sage.runway_samples WHERE subject = '" + liveEventsSeq + "'",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'sequence', $1, 'e1', now() - make_interval(days => k),
		       1.9e9 - k * 1e6, 1.9e9 - k * 1e6, 2147483647
		  FROM generate_series(0, 39) AS k ORDER BY k`, liveEventsSeq); err != nil {
		t.Fatalf("seed the identity samples: %v", err)
	}
}

// identityPack states review02d.events as the pack's schema section does.
func identityPack(t *testing.T) *workpack.Pack {
	t.Helper()
	pack, err := reviewtest.Build(reviewtest.Spec{Database: "db1",
		Window: workpack.Window{From: time.Now().UTC().AddDate(0, 0, -90),
			To: time.Now().UTC()},
		Schema: &workpack.Schema{TablesTotal: 1, Tables: []workpack.TableFact{{
			ID: "schema/table/review02d.events", Table: "review02d.events", Kind: "table",
			Columns:     []workpack.ColumnFact{{Name: "id", Type: "integer", NotNull: true}},
			Constraints: []string{"events_pkey PRIMARY KEY (id)"}}}}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return pack
}
