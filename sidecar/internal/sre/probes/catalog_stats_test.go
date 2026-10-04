package probes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The pg_stat views the tool-calling investigator may read (roadmap
// 2.1): the current database's counters, the busiest user tables and
// the top statements by time. Catalog probes like the others: typed,
// capped, read-only, never query text.

func statViewIDs() []ID { return []ID{StatDatabase, StatTables, StatStatements} }

func TestCatalog_StatViewProbes(t *testing.T) {
	for _, id := range statViewIDs() {
		spec, ok := Catalog().Spec(id)
		if !ok {
			t.Fatalf("catalog lacks %s", id)
		}
		if spec.Family != FamilyStats || spec.Args != ArgsNone || spec.Version != "v1" {
			t.Errorf("%s = family %q args %d version %s", id, spec.Family, spec.Args,
				spec.Version)
		}
	}
	st, _ := Catalog().Spec(StatStatements)
	if st.Extension != "pg_stat_statements" {
		t.Fatalf("stat_statements extension = %q", st.Extension)
	}
	if got := StatView("database"); got != StatDatabase {
		t.Fatalf("StatView(database) = %s", got)
	}
	for view, want := range map[string]ID{"tables": StatTables, "statements": StatStatements,
		"": "", "pg_authid": "", "DATABASE": ""} {
		if got := StatView(view); got != want {
			t.Errorf("StatView(%q) = %q, want %q", view, got, want)
		}
	}
	if len(StatViews()) != len(statViewIDs()) {
		t.Fatalf("StatViews() = %v", StatViews())
	}
}

func statPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func TestCatalog_StatDatabaseReadsTheCurrentDatabase(t *testing.T) {
	pool, ctx := statPool(t)
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, StatDatabase, Args{})
	if res.Status != StatusOK || len(res.Rows) != 1 {
		t.Fatalf("stat_database = %+v", res)
	}
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	row := res.Rows[0]
	if row["database"] != db {
		t.Fatalf("database = %v, want %s", row["database"], db)
	}
	for _, col := range []string{"xact_commit", "xact_rollback", "deadlocks", "temp_files",
		"blks_hit", "blks_read", "numbackends"} {
		if _, ok := row[col].(int64); !ok {
			t.Errorf("%s = %#v, want an integer", col, row[col])
		}
	}
}

func TestCatalog_StatTablesListsAUserTable(t *testing.T) {
	pool, ctx := statPool(t)
	name := fmt.Sprintf("sre_stat_%d", time.Now().UnixNano()%1_000_000_000)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+ident+" (id int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+ident) })
	if _, err := pool.Exec(ctx, "INSERT INTO "+ident+
		" SELECT g FROM generate_series(1, 100) g"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+ident+" WHERE id <= 60"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Cumulative statistics reach the views at transaction end; wait for
	// this table's counters before reading.
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, StatTables, Args{})
		if !res.Status.Usable() {
			t.Fatalf("stat_tables = %+v", res)
		}
		for _, row := range res.Rows {
			if row["relation"] == "public."+name {
				if row["n_dead_tup"].(int64) < 1 {
					break
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stat_tables never listed public.%s with dead tuples: %+v", name, res.Rows)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestCatalog_StatStatementsHasNoQueryText(t *testing.T) {
	pool, ctx := statPool(t)
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, StatStatements, Args{})
	switch {
	case res.Status == StatusUnsupported && res.Reason == "extension_not_installed":
		t.Fatalf("pg_stat_statements is not installed in the fixture database")
	case !res.Status.Usable():
		t.Fatalf("stat_statements = %+v", res)
	}
	for _, row := range res.Rows {
		if _, ok := row["query"]; ok {
			t.Fatalf("a stat_statements row carries query text: %+v", row)
		}
		if _, ok := row["queryid"]; !ok {
			t.Fatalf("a stat_statements row lacks its queryid: %+v", row)
		}
	}
}
