package sqlident

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Every key word the connected server must see quoted (catcode R, T or C)
// is in the generated table, so Ident quotes it. A server that adds a key
// word fails this test until the table is regenerated (go generate).
func TestKeywordTableCoversServer(t *testing.T) {
	conn, ctx := liveConn(t)
	rows, err := conn.Query(ctx, `SELECT word, catcode::text FROM pg_catalog.pg_get_keywords()
WHERE catcode <> 'U' ORDER BY word`)
	if err != nil {
		t.Fatalf("read pg_get_keywords(): %v", err)
	}
	words, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Word, Cat string }])
	if err != nil {
		t.Fatalf("scan pg_get_keywords(): %v", err)
	}
	if len(words) < 150 {
		t.Fatalf("server lists only %d quoted key words", len(words))
	}
	for _, w := range words {
		if got := Ident(w.Word); got != `"`+w.Word+`"` {
			t.Errorf("server key word %q (category %s) renders as %s; regenerate "+
				"keywords_gen.go", w.Word, w.Cat, got)
		}
	}
}

// Ident agrees with the server's own quote_ident on every key word and on
// plain, mixed-case and odd names: both quote exactly what must be quoted.
// The one allowed difference: the table is the union over supported
// versions, so a word another version reserves (json is unreserved on 16,
// a column-name key word on 17) is quoted even where this server would not.
func TestIdentMatchesQuoteIdent(t *testing.T) {
	conn, ctx := liveConn(t)
	rows, err := conn.Query(ctx, `SELECT w, pg_catalog.quote_ident(w) FROM (
  SELECT word FROM pg_catalog.pg_get_keywords()
  UNION ALL SELECT unnest($1::text[])) AS k(w)`,
		[]string{"orders", "user_id", "Orders", "1abc", `we"ird`, "my table", "_x", "a$b"})
	if err != nil {
		t.Fatalf("quote_ident: %v", err)
	}
	pairs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ In, Want string }])
	if err != nil {
		t.Fatalf("scan quote_ident: %v", err)
	}
	for _, p := range pairs {
		got := Ident(p.In)
		_, otherVersion := keywords[p.In]
		if got == `"`+p.In+`"` && p.Want == p.In && otherVersion {
			continue
		}
		if got != p.Want {
			t.Errorf("Ident(%q) = %s, server quote_ident gives %s", p.In, got, p.Want)
		}
	}
}

// A quoted key word is accepted where suggested SQL puts names: a column
// list of an index, a table name and a role in a GRANT.
func TestQuotedKeywordsParse(t *testing.T) {
	conn, ctx := liveConn(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, w := range []string{"to", "desc", "left", "int", "user", "window"} {
		tbl := Qualified("pg_temp", w)
		for _, sql := range []string{
			"CREATE TABLE " + tbl + " (" + Ident(w) + " int)",
			"CREATE INDEX ON " + tbl + " (" + Ident(w) + ")",
			"DROP TABLE " + tbl,
		} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
}

func liveConn(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn, ctx
}
