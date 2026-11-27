package extschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Happy path: the three extensions in schema ext, read on a session pinned
// to pg_catalog, pg_temp, are found and resolve qualified.
func TestDetectNonDefaultSchema(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.ExtSchemaDatabase(t, "extschema")
	got, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := Schemas{Statements: "ext", Pgstattuple: "ext", HypoPG: "ext"}
	if got != want {
		t.Fatalf("Detect = %+v, want %+v", got, want)
	}
	rel, err := StatementsRel(ctx, pool, "pg_stat_statements")
	if err != nil {
		t.Fatalf("StatementsRel: %v", err)
	}
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+rel).Scan(&n); err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	fn, err := Func(got.Pgstattuple, "pgstattuple_approx")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	var tableLen int64
	err = pool.QueryRow(ctx, "SELECT table_len FROM "+fn+
		"('pg_catalog.pg_class'::pg_catalog.regclass)").Scan(&tableLen)
	if err != nil || tableLen <= 0 {
		t.Fatalf("%s = %d, %v; want a measured length", fn, tableLen, err)
	}
	if _, err := pool.Exec(ctx, "SELECT count(*) FROM pg_stat_statements"); err == nil {
		t.Fatal("bare pg_stat_statements resolved on the pinned session; the test proves nothing")
	}
}

// Invalid input: schema names with a quote or a dot are quoted by Rel and
// still resolve.
func TestRelResolvesAwkwardSchemaNames(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "extschemaodd")
	for _, schema := range []string{`ext"x`, "a.b"} {
		t.Run(schema, func(t *testing.T) {
			for _, sql := range []string{"CREATE SCHEMA " + quoteIdent(schema),
				"ALTER EXTENSION pg_stat_statements SET SCHEMA " + quoteIdent(schema)} {
				if _, err := admin.Exec(ctx, sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			rel, err := StatementsRel(ctx, pool, "pg_stat_statements_info")
			if err != nil {
				t.Fatalf("StatementsRel: %v", err)
			}
			var dealloc int64
			if err := pool.QueryRow(ctx, "SELECT dealloc FROM "+rel).Scan(&dealloc); err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
		})
	}
}

// Nil/empty: an extension that is not installed reads as an empty schema,
// and its relation as the undefined_table error the bare name raised.
func TestDetectAbsentExtension(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "extschemaabsent")
	if _, err := admin.Exec(ctx, "DROP EXTENSION pgstattuple"); err != nil {
		t.Fatalf("drop pgstattuple: %v", err)
	}
	got, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Pgstattuple != "" || got.Statements != "ext" || got.HypoPG != "ext" {
		t.Fatalf("Detect = %+v, want pgstattuple absent and the others in ext", got)
	}
	_, err = Func(got.Pgstattuple, "pgstattuple_approx")
	var pgErr *pgconn.PgError
	if !errors.Is(err, ErrExtensionAbsent) || !errors.As(err, &pgErr) || pgErr.Code != "42883" {
		t.Fatalf("Func on absent pgstattuple: %v, want ErrExtensionAbsent and 42883", err)
	}
}

// Boundary: an extension created after a Detect is found by the next one.
func TestDetectSeesExtensionCreatedLater(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "extschemalater")
	if _, err := admin.Exec(ctx, "DROP EXTENSION hypopg"); err != nil {
		t.Fatalf("drop hypopg: %v", err)
	}
	before, err := Detect(ctx, pool)
	if err != nil || before.HypoPG != "" {
		t.Fatalf("Detect before = %+v, %v; want hypopg absent", before, err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION hypopg SCHEMA public"); err != nil {
		t.Fatalf("create hypopg: %v", err)
	}
	after, err := Detect(ctx, pool)
	if err != nil || after.HypoPG != "public" {
		t.Fatalf("Detect after = %+v, %v; want hypopg in public", after, err)
	}
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
