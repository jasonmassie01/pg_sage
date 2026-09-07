package vectorlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m.Run, "vectorlab")) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(t.Context(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.Exec(t.Context(), "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Fatal(err)
	}
	return p
}

func seedVectors(t *testing.T, p *pgxpool.Pool) Manifest {
	t.Helper()
	m := validManifest()
	m.Table = fmt.Sprintf("vectors_%d", time.Now().UnixNano())
	m.MaxP95MS = 5000
	m.StatementTimeoutMS, m.TotalTimeoutMS = 5000, 60000
	m.FilterColumns = []string{"tenant"}
	for i := range m.Queries {
		m.Queries[i].Filters = []string{"1"}
		m.Queries[i].Vector = []float64{float64(i*13) + .12, .3}
	}
	table := pgx.Identifier{m.Schema, m.Table}.Sanitize()
	statements := []string{
		"CREATE TABLE " + table + " (id int PRIMARY KEY, tenant int, embedding vector(2))",
		"INSERT INTO " + table + " SELECT i, 1, ('[' || i || ',0]')::vector " +
			"FROM generate_series(1,10000) i",
		"CREATE INDEX ON " + table + " USING hnsw (embedding vector_l2_ops)",
		"ANALYZE " + table,
	}
	for _, sql := range statements {
		if _, err := p.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), "DROP TABLE "+table); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestPostgresFilteredEvidenceAndLocalSettings(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	r, err := Run(t.Context(), p, m)
	if err != nil || r.Recommendation != "balanced" || len(r.ManifestSHA256) != 64 ||
		r.Snapshot == "" || r.VectorVersion == "" || r.Variants[0].MinRecall != 1 {
		t.Fatalf("real experiment: %#v %v", r, err)
	}
	var readOnly, isolation string
	if err := p.QueryRow(t.Context(), "SHOW transaction_read_only").Scan(&readOnly); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(t.Context(), "SHOW transaction_isolation").Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if readOnly != "off" || isolation != "read committed" || p.Stat().AcquiredConns() != 0 {
		t.Fatalf("leaked transaction/GUC: %s %s %#v", readOnly, isolation, p.Stat())
	}
}

func TestPostgresNoIndexEmptyAndMissingTable(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	m.Table += "_missing"
	if _, err := Run(t.Context(), p, m); err == nil || !strings.Contains(err.Error(), "42P01") {
		t.Fatalf("missing table must retain SQLSTATE: %v", err)
	}
	m.Table = strings.TrimSuffix(m.Table, "_missing")
	for i := range m.Queries {
		m.Queries[i].Filters = []string{"999"}
	}
	r, err := Run(t.Context(), p, m)
	if err != nil || r.Recommendation != "" ||
		!strings.Contains(strings.Join(r.Variants[0].Reasons, " "), "empty_ground_truth") {
		t.Fatalf("empty truth: %#v %v", r, err)
	}
	index := pgx.Identifier{m.Schema, m.Table + "_embedding_idx"}.Sanitize()
	if _, err := p.Exec(t.Context(), "DROP INDEX "+index); err != nil {
		t.Fatal(err)
	}
	for i := range m.Queries {
		m.Queries[i].Filters = []string{"1"}
	}
	r, err = Run(t.Context(), p, m)
	if err != nil || r.Recommendation != "" ||
		!strings.Contains(strings.Join(r.Variants[0].Reasons, " "), "hnsw_not_used") {
		t.Fatalf("no index: %#v %v", r, err)
	}
}

func TestPostgresCancellationAndConcurrentSessions(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, p, m); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := Run(t.Context(), p, m)
			if err != nil || r.Recommendation != "balanced" {
				t.Errorf("parallel experiment: %#v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if p.Stat().AcquiredConns() != 0 {
		t.Fatal("connection leaked")
	}
}

func TestPostgresLockTimeoutIsBounded(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	m.StatementTimeoutMS = 25
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) // Cleanup must run after failed experiment.
	table := pgx.Identifier{m.Schema, m.Table}.Sanitize()
	if _, err := tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Run(t.Context(), p, m); err == nil ||
		(!strings.Contains(err.Error(), "55P03") && !strings.Contains(err.Error(), "57014")) {
		t.Fatalf("timeout lacks distinguishable SQLSTATE: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout budget not enforced")
	}
}
