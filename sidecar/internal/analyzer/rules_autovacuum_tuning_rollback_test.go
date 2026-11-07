package analyzer

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// vacuum-07: the analyzer is the single autovacuum tuner, and its rollback
// restores a DBA's explicit prior scale factor as stored, not the global
// default. No concurrency test: the rule is a pure function of its
// snapshot.

func avRollback(t *testing.T, opts []string) string {
	t.Helper()
	got := avFindings(t, &collector.Snapshot{
		Tables: []collector.TableStats{avChurning(opts)}})
	if len(got) != 1 {
		t.Fatalf("reloptions %q: %d findings, want 1", opts, len(got))
	}
	return got[0].RollbackSQL
}

func TestAutovacuumRollbackKeepsStoredText(t *testing.T) {
	const prefix = `ALTER TABLE "public"."events" SET (autovacuum_vacuum_scale_factor = `
	cases := map[string]string{
		"0.10": "0.10",
		"0.2":  "0.2",
		"1e-1": "1e-1",
		".5":   ".5",
		"1":    "1",
		// Whitespace PostgreSQL accepted inside a quoted value is kept:
		// a bare rendering would drop it and change the stored text.
		"0.10 ": "'0.10 '",
		" 0.10": "' 0.10'",
		// Parseable but not a plain decimal (strtod accepts hex): quoted,
		// so PostgreSQL stores it verbatim and validates it on apply.
		"0x1p-1": "'0x1p-1'",
	}
	for stored, literal := range cases {
		opts := []string{"fillfactor=90", "autovacuum_vacuum_scale_factor=" + stored}
		if got, want := avRollback(t, opts), prefix+literal+");"; got != want {
			t.Errorf("stored %q: RollbackSQL = %q, want %q", stored, got, want)
		}
	}
}

// Nil reloptions are the normal untuned state: a finding whose rollback
// is RESET (rules-02 collects the column for every table).
func TestAutovacuumRollbackNilReloptionsResets(t *testing.T) {
	want := `ALTER TABLE "public"."events" RESET (autovacuum_vacuum_scale_factor);`
	if got := avRollback(t, nil); got != want {
		t.Fatalf("RollbackSQL = %q, want %q", got, want)
	}
}

// A malformed prior value leaves the DBA's intent unknown: no finding at
// all (rules-02 fails closed), so no rollback can be built from it.
func TestAutovacuumRollbackMalformedPriorNoFinding(t *testing.T) {
	for _, stored := range []string{"abc", "0.1'); DROP TABLE x; --", "", "NaN"} {
		snap := &collector.Snapshot{Tables: []collector.TableStats{
			avChurning([]string{"autovacuum_vacuum_scale_factor=" + stored})}}
		if got := avFindings(t, snap); len(got) != 0 {
			t.Errorf("stored %q: finding %q, want none", stored, got[0].RollbackSQL)
		}
	}
}

func TestAutovacuumReloptionLiteralBoundaries(t *testing.T) {
	cases := map[string]string{
		"0": "0", "0.": "0.", "00.010": "00.010", "1E+2": "1E+2",
		"+0.1": "'+0.1'", "1e": "'1e'", "Infinity": "'Infinity'", "1_0": "'1_0'",
		"it's": "'it''s'", "10": "10", "007": "'007'", "007.5": "007.5", "00e1": "00e1",
	}
	for value, want := range cases {
		if got := autovacuumReloptionLiteral(value); got != want {
			t.Errorf("autovacuumReloptionLiteral(%q) = %q, want %q", value, got, want)
		}
	}
}

// Integration: apply the finding, then its rollback, on real PostgreSQL;
// pg_class.reloptions holds the same entries as before (ALTER TABLE SET
// appends a changed key, so the order may differ; the text may not).
func TestAutovacuumRollbackRestoresReloptionsOnRealPostgres(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS vacuum07_rb CASCADE")
	})
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS vacuum07_rb CASCADE;
		CREATE SCHEMA vacuum07_rb;
		CREATE TABLE vacuum07_rb.tuned (id int)
		  WITH (autovacuum_vacuum_scale_factor = 0.10, fillfactor = 90);
		CREATE TABLE vacuum07_rb.untuned (id int) WITH (fillfactor = 90);
		CREATE TABLE vacuum07_rb.spaced (id int)
		  WITH (autovacuum_vacuum_scale_factor = ' 0.10 ');
		CREATE TABLE vacuum07_rb.hexed (id int)
		  WITH (autovacuum_vacuum_scale_factor = '0x1p-1');
		CREATE TABLE vacuum07_rb.padded (id int)
		  WITH (autovacuum_vacuum_scale_factor = '007');
		CREATE TABLE vacuum07_rb.bare (id int)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, table := range []string{"tuned", "untuned", "spaced", "hexed", "padded", "bare"} {
		before := readReloptions(t, ctx, pool, table)
		stats := avChurning(before)
		stats.SchemaName, stats.RelName = "vacuum07_rb", table
		got := avFindings(t, &collector.Snapshot{Tables: []collector.TableStats{stats}})
		if len(got) != 1 {
			t.Fatalf("%s: %d findings, want 1", table, len(got))
		}
		if _, err := pool.Exec(ctx, got[0].RecommendedSQL); err != nil {
			t.Fatalf("%s: apply %q: %v", table, got[0].RecommendedSQL, err)
		}
		applied := readReloptions(t, ctx, pool, table)
		if !containsOpt(applied, "autovacuum_vacuum_scale_factor=0.05") {
			t.Fatalf("%s: applied reloptions = %q, want scale factor 0.05", table, applied)
		}
		if _, err := pool.Exec(ctx, got[0].RollbackSQL); err != nil {
			t.Fatalf("%s: rollback %q: %v", table, got[0].RollbackSQL, err)
		}
		after := readReloptions(t, ctx, pool, table)
		if strings.Join(sorted(after), ",") != strings.Join(sorted(before), ",") {
			t.Fatalf("%s: reloptions after rollback = %q, want %q", table, after, before)
		}
	}
}

func readReloptions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	var opts []string
	err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
		WHERE oid = to_regclass('vacuum07_rb.' || quote_ident($1))`, table).Scan(&opts)
	if err != nil {
		t.Fatalf("read reloptions of %s: %v", table, err)
	}
	return opts
}

func containsOpt(opts []string, want string) bool {
	for _, opt := range opts {
		if opt == want {
			return true
		}
	}
	return false
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
