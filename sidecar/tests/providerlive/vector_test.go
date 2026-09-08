//go:build providerlive

package providerlive

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/vectorlab"
)

func checkVector(t *testing.T, f fixture) {
	var extSchema string
	checkError(t, "discover required vector extension", f.pool.QueryRow(t.Context(),
		`SELECT n.nspname FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`,
	).Scan(&extSchema))
	vectorType := pgx.Identifier{extSchema, "vector"}.Sanitize()
	opclass := pgx.Identifier{extSchema, "vector_l2_ops"}.Sanitize()
	f.exec(t, "CREATE TABLE "+f.table("vectors")+
		" (id int PRIMARY KEY, embedding "+vectorType+"(2))")
	f.exec(t, "INSERT INTO "+f.table("vectors")+
		" SELECT i, ('[' || i || ',0]')::"+vectorType+" FROM generate_series(1,2000) i")
	f.exec(t, "CREATE INDEX ON "+f.table("vectors")+" USING hnsw (embedding "+opclass+")")
	f.exec(t, "ANALYZE "+f.table("vectors"))
	m := vectorManifest(f.namespace)
	r, err := vectorlab.Run(t.Context(), f.pool, m)
	checkError(t, "run real vector lab", err)
	if r.QueryCount != 3 || r.AutoApply || r.Snapshot == "" ||
		len(r.ManifestSHA256) != 64 || len(r.Variants) != 1 || r.Variants[0].MinRecall != 1 {
		t.Fatal("vector lab lacks correct recall, snapshot, manifest, or safety evidence")
	}
	for _, q := range r.Variants[0].Queries {
		if q.TruthCount != m.K || len(q.IndexNames) == 0 {
			t.Fatal("vector lab did not use HNSW or return the expected exact baseline")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := vectorlab.Run(ctx, f.pool, m); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled vector run must preserve cancellation, got %T", err)
	}
	m.Table = "missing_vectors"
	if _, err := vectorlab.Run(t.Context(), f.pool, m); err == nil {
		t.Fatal("missing vector relation unexpectedly accepted")
	}
}

func vectorManifest(namespace string) vectorlab.Manifest {
	return vectorlab.Manifest{
		Schema: namespace, Table: "vectors", IDColumn: "id", VectorColumn: "embedding",
		Distance: "l2", K: 2, Repeats: 2, MinRecall: .95, MaxP95MS: 5000,
		StatementTimeoutMS: 10000, TotalTimeoutMS: 120000,
		Variants: []vectorlab.Variant{{Name: "balanced", EFSearch: 100,
			IterativeScan: "strict_order"}},
		Queries: []vectorlab.Query{
			{ID: "a", Vector: []float64{1.12, .3}},
			{ID: "b", Vector: []float64{12.12, .3}},
			{ID: "c", Vector: []float64{31.12, .3}},
		},
	}
}
