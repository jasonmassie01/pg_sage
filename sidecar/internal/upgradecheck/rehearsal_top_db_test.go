package upgradecheck

import (
	"errors"
	"testing"
)

// drill-02b: production's top statements come from pg_stat_statements,
// this database's only, queries only (utility statements are left out).

func TestIsQuery(t *testing.T) {
	cases := map[string]bool{
		"SELECT 1": true, "  (select 1)": true, "/* c */ WITH x AS (SELECT 1) SELECT 1": true,
		"insert into t values (1)": true, "UPDATE t SET a = 1": true, "TABLE t": true,
		"BEGIN": false, "SET search_path = x": false, "VACUUM t": false, "": false,
		"COMMIT": false, "-- SELECT\nSHOW work_mem": false,
	}
	for text, want := range cases {
		if got := isQuery(text); got != want {
			t.Errorf("isQuery(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestPostgresTopQueries_NilPool(t *testing.T) {
	if _, err := PostgresTopQueries(nil)(t.Context(), 5); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool = %v, want ErrNoPool", err)
	}
}

func TestPostgresTopQueriesDB_WithoutTheExtensionIsUnavailable(t *testing.T) {
	d := newDB(t, "")
	_, err := PostgresTopQueries(d.pool)(d.ctx, 5)
	if !errors.Is(err, ErrNoStatementTexts) {
		t.Fatalf("no pg_stat_statements = %v, want ErrNoStatementTexts", err)
	}
}

func TestPostgresTopQueriesDB_ReadsThisDatabasesQueries(t *testing.T) {
	d := newDB(t, "")
	var preloaded bool
	if err := d.pool.QueryRow(d.ctx, `SELECT 'pg_stat_statements' = ANY(
		string_to_array(replace(current_setting('shared_preload_libraries'), ' ', ''), ','))`).
		Scan(&preloaded); err != nil {
		t.Fatal(err)
	}
	d.exec(t, "CREATE EXTENSION pg_stat_statements", "CREATE TABLE public.t (a int)")
	for range 3 {
		d.exec(t, "SELECT count(*) FROM public.t WHERE a = 7", "BEGIN", "COMMIT")
	}
	got, err := PostgresTopQueries(d.pool)(d.ctx, 50)
	if !preloaded {
		if !errors.Is(err, ErrNoStatementTexts) {
			t.Fatalf("extension not loaded = %v, want ErrNoStatementTexts", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("PostgresTopQueries: %v", err)
	}
	seen := false
	for _, q := range got {
		if !isQuery(q.Text) {
			t.Fatalf("a utility statement is listed: %q", q.Text)
		}
		if q.Text == "SELECT count(*) FROM public.t WHERE a = $1" && q.QueryID != 0 {
			seen = true
		}
	}
	if !seen || len(got) > 50 {
		t.Fatalf("top statements %+v, want the normalized SELECT", got)
	}
	if one, err := PostgresTopQueries(d.pool)(d.ctx, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 1 = %d statements, %v", len(one), err)
	}
}
