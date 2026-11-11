package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexes-09 on real PostgreSQL: information_schema.columns.data_type is
// USER-DEFINED for vector, halfvec, geometry and every other extension
// type, so fetchColumns reports the type's name (udt_name) instead.

// colTypePool creates schema coltype_probe and returns the column types to
// declare for a vector and a geometry column. When pgvector or PostGIS is
// not installable, a same-named enum type stands in: it is USER-DEFINED in
// information_schema exactly as the extension type is.
func colTypePool(t *testing.T) (pool *pgxpool.Pool, vec, geom string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS coltype_probe CASCADE;
		CREATE SCHEMA coltype_probe`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS coltype_probe CASCADE")
	})
	vec = colTypeFor(t, pool, "vector", "vector(3)")
	geom = colTypeFor(t, pool, "postgis", "geometry")
	return pool, vec, geom
}

// colTypeFor installs ext and returns realType, or creates the enum
// stand-in named after realType when ext is not available.
func colTypeFor(t *testing.T, pool *pgxpool.Pool, ext, realType string) string {
	t.Helper()
	ctx := context.Background()
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions
		WHERE name = $1)`, ext).Scan(&ok); err != nil {
		t.Fatalf("read pg_available_extensions: %v", err)
	}
	name, _, _ := strings.Cut(realType, "(")
	ddl := "CREATE TYPE coltype_probe." + name + " AS ENUM ('x')"
	typ := "coltype_probe." + name
	if ok {
		ddl, typ = "CREATE EXTENSION IF NOT EXISTS "+ext, realType
	}
	t.Logf("%s available: %t; column type %s", ext, ok, typ)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	return typ
}

func TestFetchColumns_ReportsUserDefinedTypeNames(t *testing.T) {
	pool, vec, geom := colTypePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE coltype_probe.shops (id bigint NOT NULL,
		name text, tags text[], embedding `+vec+`, geom `+geom+`)`); err != nil {
		t.Fatalf("table: %v", err)
	}

	cols, err := fetchColumns(ctx, pool, "coltype_probe", "shops")
	if err != nil {
		t.Fatalf("fetchColumns: %v", err)
	}

	want := []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "name", Type: "text",
		IsNullable: true}, {Name: "tags", Type: "ARRAY", IsNullable: true},
		{Name: "embedding", Type: "vector", IsNullable: true},
		{Name: "geom", Type: "geometry", IsNullable: true}}
	if len(cols) != len(want) {
		t.Fatalf("columns = %+v, want %+v", cols, want)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Errorf("column %d = %+v, want %+v", i, cols[i], want[i])
		}
	}
}

// The classifier, fed the catalog's columns, tells a vector table from a
// PostGIS one that uses the same <-> operator.
func TestFetchColumns_FeedVectorWorkloadClassifier(t *testing.T) {
	pool, vec, geom := colTypePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE coltype_probe.docs (id int, embedding `+vec+`);
		CREATE TABLE coltype_probe.places (id int, geom `+geom+`)`); err != nil {
		t.Fatalf("tables: %v", err)
	}
	docs, err := fetchColumns(ctx, pool, "coltype_probe", "docs")
	if err != nil {
		t.Fatalf("fetchColumns docs: %v", err)
	}
	places, err := fetchColumns(ctx, pool, "coltype_probe", "places")
	if err != nil {
		t.Fatalf("fetchColumns places: %v", err)
	}
	ann := ClassifyVectorWorkload(QueryInfo{
		Text: "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 5"}, docs)
	if ann.Shape != VectorShapeANN {
		t.Fatalf("docs: Shape = %q, want %q (columns %+v)", ann.Shape, VectorShapeANN, docs)
	}
	knn := ClassifyVectorWorkload(QueryInfo{
		Text: "SELECT id FROM places ORDER BY geom <-> $1 LIMIT 5"}, places)
	if knn.Shape != "" {
		t.Fatalf("places: Shape = %q, want empty (columns %+v)", knn.Shape, places)
	}
}
