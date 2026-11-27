package vectorlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Live tests run against pgvector 0.8.x. No new concurrency test here: one
// experiment runs on one session, sequentially (parallel sessions are covered
// by TestPostgresCancellationAndConcurrentSessions).

func liveManifest(table string) Manifest {
	m := validManifest()
	m.Table, m.MaxP95MS = table, 5000
	m.StatementTimeoutMS, m.TotalTimeoutMS = 10000, 120000
	return m
}

func createTable(t *testing.T, p *pgxpool.Pool, table string, statements ...string) {
	t.Helper()
	for _, sql := range statements {
		if _, err := p.Exec(t.Context(), strings.ReplaceAll(sql, "$T", table)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), "DROP TABLE "+table); err != nil {
			t.Error(err)
		}
	})
}

func halfvecManifest(t *testing.T, p *pgxpool.Pool) Manifest {
	t.Helper()
	m := liveManifest(fmt.Sprintf("halfvec_%d", time.Now().UnixNano()))
	m.K, m.MinRecall = 5, .9 // 3000 rows: below ~1000 the planner prefers a seq scan.
	createTable(t, p, pgx.Identifier{m.Schema, m.Table}.Sanitize(),
		"CREATE TABLE $T (id int PRIMARY KEY, embedding halfvec(3072))",
		"INSERT INTO $T SELECT i, (SELECT array_agg(sin(i * 0.37 + j * 0.011)) "+
			"FROM generate_series(1,3072) j)::halfvec(3072) FROM generate_series(1,3000) i",
		"CREATE INDEX ON $T USING hnsw (embedding halfvec_l2_ops)",
		"ANALYZE $T")
	for i := range m.Queries {
		m.Queries[i].Vector = make([]float64, 3072)
		for j := range m.Queries[i].Vector {
			m.Queries[i].Vector[j] = math.Sin(float64(i*40+7)*0.37 + float64(j+1)*0.011)
		}
	}
	return m
}

func TestPostgresHalfvec3072IsMeasured(t *testing.T) {
	p := testPool(t)
	m := halfvecManifest(t, p)
	r, err := Run(t.Context(), p, m)
	if err != nil {
		t.Fatalf("halfvec(3072) experiment failed: %v", err)
	}
	v := r.Variants[0]
	if r.Recommendation != "balanced" || r.VectorType != "halfvec" || r.FormatVersion != 2 ||
		v.MinRecall < .9 || len(v.Queries[0].IndexNames) != 1 {
		t.Fatalf("halfvec report = %#v", r)
	}
}

// CHECK-12: the command exits 0 on the halfvec fixture with format_version 2.
func TestCommandHalfvecManifestExitsZero(t *testing.T) {
	p := testPool(t)
	m := halfvecManifest(t, p)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "halfvec.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	code := Command(t.Context(), []string{"--manifest", path}, testdb.SkipUnlessLive(t),
		&out, &diagnostic)
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("report is not JSON: %v %q", err, diagnostic.String())
	}
	if code != 0 || report["format_version"] != float64(2) ||
		report["vector_type"] != "halfvec" {
		t.Fatalf("command = %d report=%v err=%q", code, report, diagnostic.String())
	}
}

func TestPostgresVectorColumnRefusesOver2000Dimensions(t *testing.T) {
	p := testPool(t)
	m := liveManifest(fmt.Sprintf("wide_vector_%d", time.Now().UnixNano()))
	createTable(t, p, pgx.Identifier{m.Schema, m.Table}.Sanitize(),
		"CREATE TABLE $T (id int PRIMARY KEY, embedding vector(2001))")
	dimensions(&m, 2001)
	if _, err := Run(t.Context(), p, m); err == nil ||
		!strings.Contains(err.Error(), "2000") || !strings.Contains(err.Error(), "halfvec") {
		t.Fatalf("vector(2001) workload accepted: %v", err)
	}
}

func TestPostgresRejectsNonVectorColumn(t *testing.T) {
	p := testPool(t)
	m := liveManifest(fmt.Sprintf("bits_%d", time.Now().UnixNano()))
	createTable(t, p, pgx.Identifier{m.Schema, m.Table}.Sanitize(),
		"CREATE TABLE $T (id int PRIMARY KEY, embedding bit(2))")
	if _, err := Run(t.Context(), p, m); err == nil ||
		!strings.Contains(err.Error(), "vector or halfvec") {
		t.Fatalf("bit column accepted: %v", err)
	}
}

// selectiveManifest builds 40000 rows where only the 200 farthest (0.5%)
// match the filter, so a 20000-tuple iterative scan never reaches them.
func selectiveManifest(t *testing.T, p *pgxpool.Pool) Manifest {
	t.Helper()
	m := liveManifest(fmt.Sprintf("selective_%d", time.Now().UnixNano()))
	m.K, m.FilterColumns = 5, []string{"tenant"}
	createTable(t, p, pgx.Identifier{m.Schema, m.Table}.Sanitize(),
		"CREATE TABLE $T (id int PRIMARY KEY, tenant int, embedding vector(2))",
		"INSERT INTO $T SELECT i, (i > 39800)::int, ('[' || i || ',0]')::vector "+
			"FROM generate_series(1,40000) i",
		"CREATE INDEX ON $T USING hnsw (embedding vector_l2_ops)",
		"ANALYZE $T")
	for i := range m.Queries {
		m.Queries[i].Filters = []string{"1"}
		m.Queries[i].Vector = []float64{float64(i*13) + .12, .3}
	}
	m.Variants = []Variant{
		{Name: "narrow", EFSearch: 40, IterativeScan: "strict_order"},
		{Name: "wide", EFSearch: 100, IterativeScan: "relaxed_order",
			MaxScanTuples: intPtr(200000)},
	}
	return m
}

func TestPostgresSelectiveFilterNeedsAWiderScan(t *testing.T) {
	p := testPool(t)
	m := selectiveManifest(t, p)
	r, err := Run(t.Context(), p, m)
	if err != nil {
		t.Fatal(err)
	}
	narrow, wide := r.Variants[0], r.Variants[1]
	if narrow.Qualified || !contains(narrow.Reasons, "underfilled") {
		t.Fatalf("a 20000-tuple scan must underfill a 0.5 percent filter: %#v", narrow)
	}
	if !wide.Qualified || wide.MinRecall < m.MinRecall || r.Recommendation != "wide" {
		t.Fatalf("200000-tuple relaxed scan must qualify: %#v rec=%q", wide, r.Recommendation)
	}
}

func TestPostgresDuplicateEmbeddingsStillQualify(t *testing.T) {
	p := testPool(t)
	m := liveManifest(fmt.Sprintf("duplicates_%d", time.Now().UnixNano()))
	m.K = 3
	createTable(t, p, pgx.Identifier{m.Schema, m.Table}.Sanitize(),
		"CREATE TABLE $T (id int PRIMARY KEY, embedding vector(2))",
		"INSERT INTO $T SELECT i, ('[' || (i / 2) || ',0]')::vector "+
			"FROM generate_series(1,10000) i",
		"CREATE INDEX ON $T USING hnsw (embedding vector_l2_ops)",
		"ANALYZE $T")
	for i := range m.Queries {
		m.Queries[i].Vector = []float64{float64(i*100 + 10), .3}
	}
	r, err := Run(t.Context(), p, m)
	if err != nil {
		t.Fatal(err)
	}
	v := r.Variants[0]
	if r.Recommendation != "balanced" || v.MinRecall != 1 || r.AmbiguousBoundaryTies != 3 ||
		!v.Queries[0].Ambiguous || contains(v.Reasons, "ambiguous_boundary_tie") {
		t.Fatalf("duplicate embeddings never qualify: %#v", r)
	}
}
