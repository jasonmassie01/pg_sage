package probes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// forecast-05 on real PostgreSQL: a descending sequence is measured
// against the higher of its MINVALUE and its owning column's minimum,
// in the direction of travel; a CYCLE descending one is listed with its
// cycle flag (the runway skips it). forecast-10: a kept sequence stays
// listed although SequenceTopN others are nearer their limit.

func sequencesIn(t *testing.T, res Result, sch string) map[string]SequenceRunway {
	t.Helper()
	ss, err := Sequences(res)
	if err != nil {
		t.Fatalf("sequences: %v (%s %s)", err, res.Status, res.Reason)
	}
	got := map[string]SequenceRunway{}
	for _, s := range ss {
		if strings.HasPrefix(s.Sequence, sch+".") {
			got[strings.TrimPrefix(s.Sequence, sch+".")] = s
		}
	}
	return got
}

func TestSequenceRunway_CoversDescendingSequences(t *testing.T) {
	pool, ctx := livePool(t)
	sch := fmt.Sprintf("sre_down_%d", time.Now().UnixNano())
	q := pgx.Identifier{sch}.Sanitize()
	ddl := strings.ReplaceAll(`CREATE SCHEMA S;
		CREATE TABLE S.t (id int);
		CREATE SEQUENCE S.down_int INCREMENT -1 OWNED BY S.t.id;
		SELECT setval('S.down_int', -2100000000);
		CREATE SEQUENCE S.down_min INCREMENT -1 MINVALUE -1000 MAXVALUE -1;
		SELECT setval('S.down_min', -901);
		CREATE SEQUENCE S.down_cycle AS smallint INCREMENT -1 CYCLE;
		SELECT setval('S.down_cycle', -32000);
		CREATE SEQUENCE S.down_fresh INCREMENT -1;`, "S.", q+".")
	ddl = strings.ReplaceAll(ddl, "SCHEMA S;", "SCHEMA "+q+";")
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("sequences: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE") })
	got := sequencesIn(t, catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{}), sch)
	d, ok := got["down_int"]
	// -1 to -2147483648 is the int4 column's range; -2100000000 is 97.8% of it.
	want := (-1.0 + 2100000000) / (-1.0 + 2147483648)
	if !ok || !d.Descending() || d.Limit != -2147483648 || d.OwnerTypeMin != -2147483648 ||
		d.BindingLimit() != LimitColumnType || d.LastValue != -2100000000 ||
		d.Fraction < want-1e-9 || d.Fraction > want+1e-9 {
		t.Fatalf("down_int = %+v (%v), want limit -2147483648 at %.6f", d, ok, want)
	}
	m, ok := got["down_min"]
	// -1 to -1000 is 999 steps; -901 is 900 of them.
	if !ok || m.Limit != -1000 || m.BindingLimit() != LimitExplicitMax ||
		m.Fraction < 900.0/999-1e-9 || m.Fraction > 900.0/999+1e-9 {
		t.Fatalf("down_min = %+v (%v), want limit -1000 at 900/999", m, ok)
	}
	if c, ok := got["down_cycle"]; !ok || !c.Cycle || c.Limit != -32768 {
		t.Fatalf("down_cycle = %+v (%v), want a cycling smallint at -32768", c, ok)
	}
	if f, ok := got["down_fresh"]; ok {
		t.Fatalf("a never-called descending sequence is listed: %+v", f)
	}
}

// keepFixture creates SequenceTopN+5 sequences nearer their limit than
// the one sequence "low", which is barely used.
func keepFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sch string) string {
	t.Helper()
	q := pgx.Identifier{sch}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %[1]s;
		DO $$ BEGIN FOR i IN 1..%[2]d LOOP
		  EXECUTE format('CREATE SEQUENCE %[1]s.hot_%%s MAXVALUE 1000', i);
		  EXECUTE format('SELECT setval(%%L, 990)', '%[1]s.hot_' || i);
		END LOOP; END $$;
		CREATE SEQUENCE %[1]s.low MAXVALUE 1000;
		SELECT setval('%[1]s.low', 2);`, q, SequenceTopN+5)); err != nil {
		t.Fatalf("keep fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE") })
	return sch + ".low" // as the probe renders it (a lowercase name needs no quotes)
}

func TestSequenceRunway_KeepListOutlivesTheTopN(t *testing.T) {
	pool, ctx := livePool(t)
	sch := fmt.Sprintf("sre_keep_%d", time.Now().UnixNano())
	low := keepFixture(t, ctx, pool, sch)
	plain := catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{})
	if _, ok := sequencesIn(t, plain, sch)["low"]; ok {
		t.Fatalf("low listed without a keep-list; the fixture does not push it out")
	}
	if n := len(plain.Rows); n != SequenceTopN || plain.Truncated {
		t.Fatalf("plain read returned %d rows (truncated %v), want the top %d", n,
			plain.Truncated, SequenceTopN)
	}
	if c, err := SequenceCoverageOf(plain); err != nil || !c.Truncated {
		t.Fatalf("plain coverage = %+v (%v), want the cut listing reported", c, err)
	}
	kept := catalogRun(t, ctx, pool, SequenceRunwayProbe,
		Args{Keep: []string{low, "public.no_such_sequence"}})
	got := sequencesIn(t, kept, sch)
	if l, ok := got["low"]; !ok || l.LastValue != 2 || l.Limit != 1000 {
		t.Fatalf("kept sequence = %+v (%v), want it listed with its reading", l, ok)
	}
	if n := len(kept.Rows); n != SequenceTopN+1 || kept.Truncated {
		t.Fatalf("kept read returned %d rows (truncated %v), want the top %d plus low",
			n, kept.Truncated, SequenceTopN)
	}
	c, err := SequenceCoverageOf(kept)
	if err != nil || c.Used < SequenceTopN+6 {
		t.Fatalf("coverage = %+v (%v)", c, err)
	}
}
