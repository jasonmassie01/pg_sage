package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-11, end to end on a real server: the collector reads a descending
// sequence with a custom MINVALUE, a CYCLE sequence and an ascending int4
// sequence, and the rule judges each by the collector's pct_used.
func TestSequenceExhaustion_RealCatalog(t *testing.T) {
	ctx := context.Background()
	pool := sequenceFixturePool(t, ctx)
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	snap := collectOnce(t, ctx, collector.New(pool, cfg, version, noopLog))
	seqs := map[string]collector.SequenceStats{}
	for _, s := range snap.Sequences {
		seqs[s.SchemaName+"."+s.SequenceName] = s
	}
	assertCollected(t, seqs["public.r11_down"], -1000, false, 94.99)
	assertCollected(t, seqs["public.r11_cycle"], 1, true, 95.96)
	assertCollected(t, seqs["public.r11_up"], 1, false, 90.99)

	ff := ruleSequenceExhaustion(snap, nil, phase2Config(), nil)
	if f := findingFor(ff, "public.r11_cycle"); f != nil {
		t.Fatalf("CYCLE sequence finding %+v, want none", f)
	}
	for ident, pct := range map[string]float64{
		"public.r11_down": seqs["public.r11_down"].PctUsed,
		"public.r11_up":   seqs["public.r11_up"].PctUsed,
	} {
		f := findingFor(ff, ident)
		if f == nil || f.Severity != "critical" {
			t.Fatalf("%s finding = %+v, want critical", ident, f)
		}
		if f.Detail["usage_pct"] != pct ||
			f.Detail["runway_category"] != "forecast_sequence_runway" {
			t.Fatalf("%s detail = %+v, want usage_pct %.2f (the collector's) and "+
				"the runway category", ident, f.Detail, pct)
		}
	}
}

// sequenceFixturePool is a bootstrapped database holding the three
// rules-11 sequences: descending with a custom MINVALUE, CYCLE, ascending.
func sequenceFixturePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rules11_seq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE SEQUENCE public.r11_down INCREMENT -1 MINVALUE -1000 MAXVALUE -1 START -1;
		SELECT setval('public.r11_down', -950);
		CREATE SEQUENCE public.r11_cycle AS integer MINVALUE 1 MAXVALUE 100 CYCLE;
		SELECT setval('public.r11_cycle', 96);
		CREATE SEQUENCE public.r11_up AS integer MINVALUE 1 MAXVALUE 1000;
		SELECT setval('public.r11_up', 910);`); err != nil {
		t.Fatalf("create sequences: %v", err)
	}
	return pool
}

func assertCollected(
	t *testing.T, s collector.SequenceStats, minValue int64, cycle bool, pct float64,
) {
	t.Helper()
	if s.SequenceName == "" {
		t.Fatalf("sequence with min %d not collected", minValue)
	}
	if s.MinValue != minValue || s.Cycle != cycle || s.PctUsed != pct {
		t.Fatalf("%s collected as min %d cycle %v pct %.2f, want %d %v %.2f",
			s.SequenceName, s.MinValue, s.Cycle, s.PctUsed, minValue, cycle, pct)
	}
}
