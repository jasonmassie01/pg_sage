//go:build cgo

package impact

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// corpusEntry is one migration of the review corpus. post is the
// post-condition kind its DDL lands as ("" for statements that are not
// DDL). Applied in order, every entry succeeds on a fresh rv copy.
type corpusEntry struct {
	sql  string
	post string
}

var corpus = []corpusEntry{
	{"CREATE INDEX CONCURRENTLY rv_orders_status_idx ON rv.rv_orders (status)",
		changereview.PostIndexValid},
	{"DROP INDEX CONCURRENTLY rv.rv_orders_status_idx", changereview.PostIndexAbsent},
	{"ALTER TABLE rv.rv_orders ADD COLUMN note text", changereview.PostColumnPresent},
	{"ALTER TABLE rv.rv_orders RENAME COLUMN note TO note2", changereview.PostColumnRenamed},
	{"ALTER TABLE rv.rv_orders ALTER COLUMN code TYPE varchar(40)",
		changereview.PostColumnType},
	{"ALTER TABLE rv.rv_orders ALTER COLUMN email SET NOT NULL", changereview.PostNotNull},
	{"ALTER TABLE rv.rv_orders ALTER COLUMN email DROP NOT NULL", changereview.PostNullable},
	{"ALTER TABLE rv.rv_orders DROP COLUMN note2", changereview.PostColumnAbsent},
	{"ALTER TABLE rv.rv_items ADD CONSTRAINT rv_items_qty_ck CHECK (qty >= 0) NOT VALID",
		changereview.PostConstraint},
	{"ALTER TABLE rv.rv_items VALIDATE CONSTRAINT rv_items_qty_ck",
		changereview.PostConstraint},
	{"ALTER TABLE rv.rv_items DROP CONSTRAINT rv_items_qty_ck",
		changereview.PostConstraintAbsent},
	{"CREATE TABLE rv.rv_new (id bigint PRIMARY KEY, at timestamptz)",
		changereview.PostRelationPresent},
	{"ALTER TABLE rv.rv_new RENAME TO rv_new2", changereview.PostRelationRenamed},
	{"DROP TABLE rv.rv_new2", changereview.PostRelationAbsent},
	{"ALTER TABLE rv.rv_items ADD CONSTRAINT rv_items_order_fk FOREIGN KEY (order_id) " +
		"REFERENCES rv.rv_orders (id) NOT VALID", changereview.PostConstraint},
	{"SET lock_timeout = '3s'", ""},
	{"UPDATE rv.rv_orders SET status = 'x' WHERE created_at < now()", ""},
	{"DELETE FROM rv.rv_items WHERE qty = 49", ""},
	{"SELECT id FROM rv.rv_items WHERE qty > $1", ""},
	{"CREATE STATISTICS rv.rv_orders_st (dependencies) ON status, customer_id " +
		"FROM rv.rv_orders", ""},
}

func corpusInput() changereview.Input {
	in := input("")
	in.SQLFiles = nil
	for _, c := range corpus {
		in.SQLFiles = append(in.SQLFiles, changereview.SQLFile{Path: "m.sql", Content: c.sql})
	}
	return in
}

// catalogDigest is the pg_class, pg_attribute, pg_index and pg_constraint
// rows of schema rv as text.
func catalogDigest(t *testing.T, e *env) string {
	t.Helper()
	var digest string
	err := e.f.Pool.QueryRow(e.ctx, `SELECT md5(concat_ws('#',
		(SELECT string_agg(c.relname || c.relkind || c.relnatts ||
		        coalesce(c.reloptions::text, ''), ',' ORDER BY c.relname)
		   FROM pg_class c WHERE c.relnamespace = 'rv'::regnamespace),
		(SELECT string_agg(a.attrelid::regclass || a.attname || a.atttypid || a.attnotnull,
		        ',' ORDER BY a.attrelid, a.attnum)
		   FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		  WHERE c.relnamespace = 'rv'::regnamespace),
		(SELECT string_agg(i.indexrelid::regclass || i.indisvalid || i.indkey::text, ','
		        ORDER BY i.indexrelid)
		   FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
		  WHERE c.relnamespace = 'rv'::regnamespace),
		(SELECT string_agg(conname || contype || convalidated, ',' ORDER BY conname)
		   FROM pg_constraint WHERE connamespace = 'rv'::regnamespace)))`).Scan(&digest)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

var writeStatement = regexp.MustCompile(
	`(?is)^\s*(/\*.*?\*/\s*)*(INSERT|UPDATE|DELETE|MERGE|TRUNCATE|CREATE|ALTER|DROP|COPY)\b`)

// CHECK-IMP-12: the engine over the whole corpus reads and plans; it
// writes nothing outside schema sage and runs no DDL.
func TestEngineIsReadOnlyOverTheCorpus(t *testing.T) {
	e := newEnv(t)
	pool, role := e.f.ReaderPool(rvReaderWithout())
	beforeOrders, beforeItems := tableChecksum(t, e, "rv.rv_orders"),
		tableChecksum(t, e, "rv.rv_items")
	beforeCatalog := catalogDigest(t, e)
	a := e.analyzeWith(e.engine(pool, nil), corpusInput())
	if len(a.Statements) != len(corpus) {
		t.Fatalf("%d statements analyzed, want %d", len(a.Statements), len(corpus))
	}
	if tableChecksum(t, e, "rv.rv_orders") != beforeOrders ||
		tableChecksum(t, e, "rv.rv_items") != beforeItems {
		t.Fatal("the review changed rv rows")
	}
	if catalogDigest(t, e) != beforeCatalog {
		t.Fatal("the review changed schema rv's catalog rows")
	}
	rows, err := e.f.Pool.Query(e.ctx, `SELECT s.query FROM pg_stat_statements s
		JOIN pg_roles r ON r.oid = s.userid WHERE r.rolname = $1
		  AND s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatal(err)
		}
		n++
		if writeStatement.MatchString(q) && !regexp.MustCompile(`\bsage\.`).MatchString(q) {
			t.Errorf("pg_sage's role ran a write or DDL outside schema sage: %q", q)
		}
	}
	if rows.Err() != nil || n == 0 {
		t.Fatalf("pg_stat_statements shows %d statements of the reviewer: %v", n, rows.Err())
	}
}

func TestFullCorpusWithinTheDeterministicDeadline(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	a, err := e.eng.Analyze(ctx, corpusInput())
	if err != nil {
		t.Fatalf("corpus Analyze: %v", err)
	}
	if took := time.Since(start); took >= 20*time.Second {
		t.Fatalf("the corpus took %v, want under 20 s", took)
	}
	checkAnalysis(t, a)
	if a.ParseMode != changereview.ParseAST || len(a.Statements) != len(corpus) {
		t.Fatalf("parse mode %s with %d statements", a.ParseMode, len(a.Statements))
	}
}
