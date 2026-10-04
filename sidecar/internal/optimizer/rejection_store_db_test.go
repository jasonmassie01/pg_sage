package optimizer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration: sage.optimizer_rejection on a real PostgreSQL, and the whole
// optimizer cycle with a real HypoPG what-if.

func rejectionDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := connectTestDB(t)
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

var rejSchemaSeq atomic.Int64

// rejSchema is a schema_name no other test writes, so tests sharing the
// package database never see each other's rows.
func rejSchema() string {
	return fmt.Sprintf("rejmem_%d_%d", time.Now().UnixNano()%1_000_000_000, rejSchemaSeq.Add(1))
}

func dbRejection(t *testing.T, schemaName, ddl string, improvement float64) rejection {
	t.Helper()
	return rejection{Schema: schemaName, Table: "ai_claims", Shape: mustShape(t, ddl), DDL: ddl,
		ImprovementPct: improvement, MinImprovementPct: 10,
		Reason: fmt.Sprintf("call-weighted improvement %.1f%% is below the 10.0%% minimum",
			improvement),
		Workload:    []workloadSample{{QueryID: 7, Calls: 1200, MeanMs: 3.5}, {QueryID: 8}},
		RowEstimate: 123456}
}

func TestPGRejectionStore_RoundTrip(t *testing.T) {
	store := newPGRejectionStore(rejectionDB(t))
	ctx := context.Background()
	s := rejSchema()
	want := dbRejection(t, s, lifeosDDL("a", "id, status"), 0.3)
	if err := store.record(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := store.recent(ctx, s, "ai_claims", 7*24*time.Hour, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("recent = %d rows, %v", len(got), err)
	}
	r := got[0]
	if r.Schema != s || r.Table != "ai_claims" || r.DDL != want.DDL ||
		r.ImprovementPct != 0.3 || r.MinImprovementPct != 10 || r.Reason != want.Reason ||
		r.RowEstimate != 123456 || r.MeasureCount != 1 {
		t.Fatalf("round trip = %+v", r)
	}
	if !reflect.DeepEqual(r.Shape, want.Shape) || r.Shape.hash() != want.Shape.hash() {
		t.Fatalf("shape round trip:\n got %+v\nwant %+v", r.Shape, want.Shape)
	}
	if !reflect.DeepEqual(r.Workload, want.Workload) {
		t.Fatalf("workload round trip = %+v", r.Workload)
	}
	if time.Since(r.MeasuredAt) > time.Minute || r.MeasuredAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("measured_at = %v, want now", r.MeasuredAt)
	}
}

func TestPGRejectionStore_UpsertSameShape(t *testing.T) {
	store := newPGRejectionStore(rejectionDB(t))
	ctx := context.Background()
	s := rejSchema()
	if err := store.record(ctx, dbRejection(t, s, lifeosDDL("a", "id, status"), 1)); err != nil {
		t.Fatal(err)
	}
	second := dbRejection(t, s, lifeosDDL("b", "status, id"), 2.5)
	second.RowEstimate = 999
	if err := store.record(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, err := store.recent(ctx, s, "ai_claims", time.Hour, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("recent = %d rows, %v; want one row per shape", len(got), err)
	}
	if got[0].MeasureCount != 2 || got[0].ImprovementPct != 2.5 || got[0].RowEstimate != 999 ||
		got[0].DDL != second.DDL {
		t.Fatalf("upsert kept stale evidence: %+v", got[0])
	}
}

func TestPGRejectionStore_SubsetIncludeIsOwnRow(t *testing.T) {
	store := newPGRejectionStore(rejectionDB(t))
	ctx := context.Background()
	s := rejSchema()
	for _, inc := range []string{"id", "id, status"} {
		if err := store.record(ctx, dbRejection(t, s, lifeosDDL("a", inc), 0)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.recent(ctx, s, "ai_claims", time.Hour, 100)
	if err != nil || len(got) != 2 {
		t.Fatalf("recent = %d rows, %v", len(got), err)
	}
	if !got[0].Shape.sameIdea(got[1].Shape) {
		t.Fatal("subset INCLUDE rows must still match each other")
	}
}

func TestPGRejectionStore_MaxAgeLimitAndOrder(t *testing.T) {
	pool := rejectionDB(t)
	store := newPGRejectionStore(pool)
	ctx := context.Background()
	s := rejSchema()
	for i, ddl := range []string{"CREATE INDEX i ON t (a)", "CREATE INDEX i ON t (b)",
		"CREATE INDEX i ON t (c)"} {
		if err := store.record(ctx, dbRejection(t, s, ddl, float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.optimizer_rejection SET measured_at =
		CASE key_cols[1] WHEN 'a' THEN now() - interval '8 days'
		                 WHEN 'b' THEN now() - interval '1 hour' ELSE now() END
		WHERE schema_name = $1`, s); err != nil {
		t.Fatal(err)
	}
	got, err := store.recent(ctx, s, "ai_claims", 7*24*time.Hour, 100)
	if err != nil || len(got) != 2 {
		t.Fatalf("7-day window = %d rows (%v), want 2", len(got), err)
	}
	if got[0].Shape.Keys[0] != "c" || got[1].Shape.Keys[0] != "b" {
		t.Fatalf("rows not newest first: %v, %v", got[0].Shape, got[1].Shape)
	}
	got, err = store.recent(ctx, s, "ai_claims", 7*24*time.Hour, 1)
	if err != nil || len(got) != 1 || got[0].Shape.Keys[0] != "c" {
		t.Fatalf("limit 1 = %+v (%v), want the newest", got, err)
	}
}

func TestPGRejectionStore_ScopedToCurrentDatabase(t *testing.T) {
	pool := rejectionDB(t)
	store := newPGRejectionStore(pool)
	ctx := context.Background()
	s := rejSchema()
	r := dbRejection(t, s, lifeosDDL("a", "id"), 0)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.optimizer_rejection (database_name,
		schema_name, table_name, shape_hash, method, key_cols, predicate, include_cols, ddl,
		improvement_pct, min_improvement_pct, reason)
		VALUES ('some_other_database', $1, 'ai_claims', $2, 'btree', $3, '', $4, $5, 0, 10, 'r')`,
		s, r.Shape.hash(), r.Shape.Keys, r.Shape.Include, r.DDL); err != nil {
		t.Fatal(err)
	}
	got, err := store.recent(ctx, s, "ai_claims", time.Hour, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("another database's rejection leaked: %d rows (%v)", len(got), err)
	}
}

func TestPGRejectionStore_ConcurrentRecordsOfOneShape(t *testing.T) {
	store := newPGRejectionStore(rejectionDB(t))
	ctx := context.Background()
	s := rejSchema()
	const writers = 8
	rows := make([]rejection, writers)
	for i := range rows {
		inc := []string{"id, status", "status, id"}[i%2]
		rows[i] = dbRejection(t, s, lifeosDDL(fmt.Sprintf("n%d", i), inc), float64(i)/10)
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(r rejection) {
			defer wg.Done()
			errs <- store.record(ctx, r)
		}(rows[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent record: %v", err)
		}
	}
	got, err := store.recent(ctx, s, "ai_claims", time.Hour, 100)
	if err != nil || len(got) != 1 || got[0].MeasureCount != writers {
		t.Fatalf("concurrent records: %d rows (%v), count %v; want 1 row counted %d",
			len(got), err, got, writers)
	}
}

func TestPGRejectionStore_ErrorsCarryContext(t *testing.T) {
	store := newPGRejectionStore(rejectionDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.recent(ctx, "s", "t", time.Hour, 10); err == nil ||
		!strings.Contains(err.Error(), "load what-if rejections") {
		t.Fatalf("recent on a canceled context: %v", err)
	}
	if err := store.record(ctx, dbRejection(t, "s", lifeosDDL("a", "id"), 0)); err == nil ||
		!strings.Contains(err.Error(), "record what-if rejection") {
		t.Fatalf("record on a canceled context: %v", err)
	}
	bare, err := pgxpool.New(context.Background(), testdb.CreateDatabase(t, "rejmem_bare"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bare.Close)
	_, err = newPGRejectionStore(bare).recent(context.Background(), "s", "t", time.Hour, 10)
	if err == nil || !strings.Contains(err.Error(), "optimizer_rejection") {
		t.Fatalf("missing table must be a distinguishable error: %v", err)
	}
}

func TestNew_RejectionMemoryFollowsConfig(t *testing.T) {
	pool := rejectionDB(t)
	cfg := fnTestOptimizerConfig()
	cfg.RejectionMemory = config.DefaultOptimizerRejectionMemory()
	if o := New(pool, cfg, 170000, noopLog2); o.memory == nil {
		t.Fatal("enabled memory with a pool was not built")
	}
	cfg.RejectionMemory.Enabled = false
	if o := New(pool, cfg, 170000, noopLog2); o.memory != nil {
		t.Fatal("disabled memory was built")
	}
}

// countingDelegate counts evaluations of a real what-if validator.
type countingDelegate struct {
	inner whatIfValidator
	calls atomic.Int32
}

func (c *countingDelegate) IsAvailable(ctx context.Context) bool { return c.inner.IsAvailable(ctx) }

func (c *countingDelegate) Validate(ctx context.Context, rec Recommendation, q []QueryInfo,
) (WhatIfResult, error) {
	c.calls.Add(1)
	return c.inner.Validate(ctx, rec, q)
}

func claimsITSetup(t *testing.T) (*pgxpool.Pool, int) {
	t.Helper()
	hypopgSessionPool(t) // installs HypoPG, or skips when the server lacks it
	pool := rejectionDB(t)
	ctx := context.Background()
	for _, sql := range []string{
		"CREATE SCHEMA IF NOT EXISTS rejmem_it",
		"DROP TABLE IF EXISTS rejmem_it.claims",
		"CREATE TABLE rejmem_it.claims (id bigint, status text, evidence text)",
		`INSERT INTO rejmem_it.claims SELECT i, CASE WHEN i % 50 = 0 THEN 'open'
			ELSE 'closed' END, md5(i::text) || md5((i + 1)::text)
			FROM generate_series(1, 20000) i`,
		"ANALYZE rejmem_it.claims",
		"DELETE FROM sage.optimizer_rejection WHERE schema_name = 'rejmem_it'",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	return pool, version
}

func claimsSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "rejmem_it", RelName: "claims",
			NLiveTup: 20000, SeqScan: 500, TableBytes: 2 << 20}},
		Queries: []collector.QueryStats{{QueryID: 910001, Calls: 5000, MeanExecTime: 4,
			TotalExecTime: 20000,
			Query:         "SELECT id FROM rejmem_it.claims WHERE status = 'open'"}},
	}
}

const claimsITDDL = "CREATE INDEX CONCURRENTLY %s ON rejmem_it.claims USING btree " +
	"(evidence text_pattern_ops) INCLUDE (%s)"

// End to end on a real database: the first admission measures the idea
// with HypoPG and remembers the rejection; the renamed variant on the
// next cycle is not measured again, and the measured shape is listed for
// the tuning agent's case packet.
func TestAdmit_RejectionMemoryWithRealHypoPG(t *testing.T) {
	pool, version := claimsITSetup(t)
	cfg := fnTestOptimizerConfig()
	cfg.MinSnapshots = 0
	cfg.RejectionMemory = config.DefaultOptimizerRejectionMemory()
	logs := &logRecorder{}
	o := New(pool, cfg, version, logs.log)
	w := &countingDelegate{inner: o.whatIf}
	o.whatIf = w
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tc, ok, err := o.TableContext(ctx, claimsSnapshot(), "rejmem_it.claims")
	if err != nil || !ok || len(tc.Queries) != 1 {
		t.Fatalf("context = %+v ok=%t err=%v", tc, ok, err)
	}
	first := o.Admit(ctx, Recommendation{DDL: fmt.Sprintf(claimsITDDL,
		"claims_evidence_pattern_idx", "id, status"), IndexType: "btree"}, tc)
	if first.Outcome != AdmitRejected || w.calls.Load() != 1 {
		t.Fatalf("first: %+v whatif=%d logs: %q", first, w.calls.Load(), logs.lines)
	}
	var improvement float64
	var reason string
	if err := pool.QueryRow(ctx, `SELECT improvement_pct, reason FROM sage.optimizer_rejection
		WHERE schema_name = 'rejmem_it' AND table_name = 'claims'`).
		Scan(&improvement, &reason); err != nil {
		t.Fatalf("rejection not persisted: %v", err)
	}
	if improvement >= 10 || !strings.Contains(reason, "below the 10.0% minimum") {
		t.Fatalf("persisted %.2f%% %q", improvement, reason)
	}
	tc, _, _ = o.TableContext(ctx, claimsSnapshot(), "rejmem_it.claims")
	second := o.Admit(ctx, Recommendation{DDL: fmt.Sprintf(claimsITDDL,
		"claims_evidence_prefix_idx", "status, id"), IndexType: "btree"}, tc)
	if second.Outcome != AdmitMeasured || w.calls.Load() != 1 ||
		o.MemoryStats().WhatIfSkipped != 1 {
		t.Fatalf("second: %+v whatif=%d stats=%+v", second, w.calls.Load(), o.MemoryStats())
	}
	lines := o.MeasuredRejections(ctx, tc)
	if len(lines) != 1 ||
		!strings.Contains(lines[0], "btree (evidence text_pattern_ops) INCLUDE (id, status)") {
		t.Fatalf("measured shapes = %q", lines)
	}
}
