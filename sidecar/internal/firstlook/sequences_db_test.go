package firstlook

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sequence runway finds the column a sequence feeds through OWNED BY,
// identity, or only a column DEFAULT nextval(...). The narrowest column
// type caps the range, and every sequence stays one row.

// seedSchema creates a unique schema, runs stmts with {s} replaced by the
// quoted schema name, and drops it when the test ends.
func seedSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	prefix string, stmts ...string) string {
	t.Helper()
	s := uniqueSchema(prefix)
	q := pgx.Identifier{s}.Sanitize()
	all := []string{"CREATE SCHEMA " + q}
	for _, st := range stmts {
		all = append(all, strings.ReplaceAll(st, "{s}", q))
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	execAll(t, ctx, pool, all...)
	return s
}

// schemaSequences reads the sequences of one schema the way Run does.
func schemaSequences(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schema string) []Sequence {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	all, err := readSequences(ctx, tx)
	if err != nil {
		t.Fatalf("read sequences: %v", err)
	}
	var out []Sequence
	for _, s := range all {
		if s.Schema == schema {
			out = append(out, s)
		}
	}
	return out
}

// The verification case, exactly: a bigint sequence at 2,140,000,000 used
// only through DEFAULT nextval by an int column, without OWNED BY.
func TestSequenceRunwayFindsColumnThroughDefault(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedSchema(t, ctx, pool, "fl_seqdef_",
		"CREATE SEQUENCE {s}.big_seq AS bigint",
		"SELECT setval('{s}.big_seq', 2140000000)",
		"CREATE TABLE {s}.events (id int DEFAULT nextval('{s}.big_seq'))")
	seqs := schemaSequences(t, ctx, pool, s)
	if len(seqs) != 1 || seqs[0].OwnerColumn != s+".events.id" ||
		seqs[0].OwnerType != "integer" {
		t.Fatalf("sequences = %+v, want big_seq feeding %s.events.id (integer)", seqs, s)
	}
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	it := findItem(r, RuleSequenceRunway, s+".big_seq")
	if it == nil || it.Severity != SeverityCritical {
		t.Fatalf("item = %+v, want a critical sequence_runway item for big_seq", it)
	}
	if !strings.Contains(it.Detail, s+".events.id") || !strings.Contains(it.Detail, "integer") ||
		!strings.Contains(it.Title, "99.7% used") ||
		!strings.Contains(it.Recommendation, "bigint") {
		t.Fatalf("item title %q detail %q recommendation %q", it.Title, it.Detail,
			it.Recommendation)
	}
	requireEvidence(t, *it)
}

// OWNED BY (serial) still caps the range, with or without a default.
func TestSequenceRunwayOwnedByStillCaps(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedSchema(t, ctx, pool, "fl_seqown_",
		"CREATE SEQUENCE {s}.owned_seq AS bigint",
		"SELECT setval('{s}.owned_seq', 2000000000)",
		"CREATE TABLE {s}.orders (id int)",
		"ALTER SEQUENCE {s}.owned_seq OWNED BY {s}.orders.id",
		"CREATE TABLE {s}.serials (id serial)",
		"SELECT setval('{s}.serials_id_seq', 5)")
	seqs := schemaSequences(t, ctx, pool, s)
	got := map[string]Sequence{}
	for _, q := range seqs {
		got[q.Name] = q
	}
	if len(seqs) != 2 || got["owned_seq"].OwnerColumn != s+".orders.id" ||
		got["owned_seq"].OwnerType != "integer" ||
		got["serials_id_seq"].OwnerColumn != s+".serials.id" {
		t.Fatalf("sequences = %+v, want one row each with its owning column", seqs)
	}
	items, _ := SequenceRunway(seqs, DefaultThresholds())
	if len(items) != 1 || items[0].Object != s+".owned_seq" ||
		items[0].Severity != SeverityCritical {
		t.Fatalf("items = %+v, want owned_seq critical only", items)
	}
}

// Several columns use one sequence: one row, capped by the narrowest type,
// whether that column owns the sequence or only defaults to it.
func TestSequenceRunwayNarrowestColumnCaps(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedSchema(t, ctx, pool, "fl_seqmany_",
		"CREATE SEQUENCE {s}.shared_seq AS bigint",
		"SELECT setval('{s}.shared_seq', 2140000000)",
		"CREATE TABLE {s}.a_wide (id bigint DEFAULT nextval('{s}.shared_seq'))",
		"ALTER SEQUENCE {s}.shared_seq OWNED BY {s}.a_wide.id",
		"CREATE TABLE {s}.b_narrow (id int DEFAULT nextval('{s}.shared_seq'))",
		"CREATE TABLE {s}.c_wide (id bigint DEFAULT nextval('{s}.shared_seq'))")
	seqs := schemaSequences(t, ctx, pool, s)
	if len(seqs) != 1 || seqs[0].OwnerColumn != s+".b_narrow.id" ||
		seqs[0].OwnerType != "integer" {
		t.Fatalf("sequences = %+v, want one row capped by %s.b_narrow.id", seqs, s)
	}
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	n := 0
	for _, it := range r.Items {
		if it.Rule == RuleSequenceRunway && strings.HasPrefix(it.Object, s+".") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d sequence items for %s, want exactly one", n, s)
	}
}

// Negative: a bigint column fed through DEFAULT does not cap a bigint
// sequence, and a sequence nobody uses has no column.
func TestSequenceRunwayDefaultBigintIsNotCapped(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedSchema(t, ctx, pool, "fl_seqbig_",
		"CREATE SEQUENCE {s}.big_seq AS bigint",
		"SELECT setval('{s}.big_seq', 2140000000)",
		"CREATE TABLE {s}.events (id bigint DEFAULT nextval('{s}.big_seq'))",
		"CREATE SEQUENCE {s}.lonely_seq",
		"SELECT setval('{s}.lonely_seq', 7)")
	seqs := schemaSequences(t, ctx, pool, s)
	got := map[string]Sequence{}
	for _, q := range seqs {
		got[q.Name] = q
	}
	if len(seqs) != 2 || got["big_seq"].OwnerType != "bigint" ||
		got["lonely_seq"].OwnerColumn != "" || got["lonely_seq"].OwnerType != "" {
		t.Fatalf("sequences = %+v", seqs)
	}
	if items, _ := SequenceRunway(seqs, DefaultThresholds()); len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
}
