package optimizer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The optimizer as the tuning agent's index toolbox (roadmap 2.2): the
// model no longer runs here; the agent hands over a candidate and the
// optimizer keeps every deterministic gate — canonical form, the
// validator, rejection memory and the HypoPG what-if.

func admissionConfig() *config.OptimizerConfig {
	return &config.OptimizerConfig{Enabled: true, MinQueryCalls: 1, MaxIndexesPerTable: 10,
		MaxNewPerTable: 3, HypoPGMinImprovePct: 10, WriteHeavyRatioPct: 70,
		WriteImpactThreshPct: 15}
}

func admissionOptimizer(store *memStore, res WhatIfResult) (*Optimizer, *countingWhatIf,
	*logRecorder) {
	logs := &logRecorder{}
	o := New(nil, admissionConfig(), logs.log)
	w := &countingWhatIf{result: res}
	o.whatIf = w
	o.memory = testMemory(store, logs)
	return o, w, logs
}

// countingWhatIf counts evaluations and returns a fixed result.
type countingWhatIf struct {
	calls  atomic.Int32
	result WhatIfResult
}

func (c *countingWhatIf) IsAvailable(context.Context) bool { return true }

func (c *countingWhatIf) Validate(context.Context, Recommendation, []QueryInfo,
) (WhatIfResult, error) {
	c.calls.Add(1)
	return c.result, nil
}

// admitTable is memTable (the rejection memory's workload) with columns
// and statement texts.
func admitTable() TableContext {
	tc := memTable()
	tc.Columns = []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "status", Type: "text"},
		{Name: "evidence_event_ids_json", Type: "text"}}
	tc.Queries[0].Text = "SELECT id FROM ai_claims WHERE status = $1"
	tc.Queries[1].Text = "SELECT status FROM ai_claims WHERE id = $1"
	tc.WriteRate, tc.IndexCount, tc.Workload = 5, 1, "oltp_read"
	tc.ColumnsKnown = true
	return tc
}

func admitNoLog(string, string, ...any) {}

func candidate(ddl string) Recommendation {
	return Recommendation{DDL: ddl, Rationale: "model rationale", IndexType: "btree"}
}

const statusDDL = "CREATE INDEX CONCURRENTLY ai_claims_status_idx ON public.ai_claims (status)"

func TestAdmit_VerifiedCandidate(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
		Improvement: 35, SizeBytes: 16384})
	a := o.Admit(context.Background(), candidate(statusDDL), admitTable())
	if a.Outcome != AdmitAccepted || w.calls.Load() != 1 {
		t.Fatalf("admission = %+v (what-ifs %d)", a, w.calls.Load())
	}
	r := a.Rec
	if r.WhatIf != WhatIfVerified || !r.Validated || r.EstimatedImprovementPct != 35 ||
		r.Category != OptimizerCategory || r.Table != "public.ai_claims" {
		t.Fatalf("rec = %+v", r)
	}
	if len(r.AffectedQueryIDs) != 2 || r.AffectedQueryIDs[0] != 1 || r.AffectedQueryIDs[1] != 2 {
		t.Fatalf("targets = %v", r.AffectedQueryIDs)
	}
	if !strings.HasPrefix(r.DropDDL, "DROP INDEX CONCURRENTLY") ||
		r.CostEstimate == nil || r.CostEstimate.EstimatedSizeBytes != 16384 {
		t.Fatalf("rollback and size: %+v", r)
	}
}

func TestAdmit_InvalidCandidateNeverReachesTheWhatIf(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2, Improvement: 50})
	for _, ddl := range []string{
		"CREATE INDEX CONCURRENTLY x ON public.ai_claims (no_such_column)",
		"CREATE INDEX x ON public.ai_claims (status)",
		"CREATE INDEX CONCURRENTLY x ON public.other_table (status)",
	} {
		a := o.Admit(context.Background(), candidate(ddl), admitTable())
		if a.Outcome != AdmitInvalid || a.Reason == "" {
			t.Fatalf("%s: %+v", ddl, a)
		}
	}
	if w.calls.Load() != 0 {
		t.Fatalf("what-ifs = %d", w.calls.Load())
	}
}

func TestAdmit_RememberedIdeaIsNotRemeasured(t *testing.T) {
	store := newMemStore(storedRejection(t, statusDDL, 0))
	o, w, _ := admissionOptimizer(store, WhatIfResult{Measured: 2, Improvement: 50})
	tc := admitTable()
	renamed := strings.Replace(statusDDL, "ai_claims_status_idx", "other_name", 1)
	a := o.Admit(context.Background(), candidate(renamed), tc)
	if a.Outcome != AdmitMeasured || w.calls.Load() != 0 {
		t.Fatalf("admission = %+v what-ifs %d", a, w.calls.Load())
	}
	if o.MemoryStats().WhatIfSkipped != 1 {
		t.Fatalf("skips = %+v", o.MemoryStats())
	}
}

func TestAdmit_WhatIfRejectionIsRemembered(t *testing.T) {
	store := newMemStore()
	o, _, _ := admissionOptimizer(store, WhatIfResult{Measured: 2, Improvement: 0,
		SizeBytes: 8192})
	a := o.Admit(context.Background(), candidate(statusDDL), admitTable())
	if a.Outcome != AdmitRejected || !strings.Contains(a.Reason, "below") {
		t.Fatalf("admission = %+v", a)
	}
	if store.records != 1 || len(o.MeasuredRejections(context.Background(),
		admitTable())) != 1 {
		t.Fatalf("records %d: the rejection is remembered and listed", store.records)
	}
}

type unavailableWhatIf struct{}

func (unavailableWhatIf) IsAvailable(context.Context) bool { return false }
func (unavailableWhatIf) Validate(context.Context, Recommendation, []QueryInfo) (
	WhatIfResult, error) {
	return WhatIfResult{}, errors.New("unreachable")
}

func TestAdmit_UnverifiedWithoutHypoPG(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	o.whatIf = unavailableWhatIf{}
	a := o.Admit(context.Background(), candidate(statusDDL), admitTable())
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfUnverified || a.Rec.Validated {
		t.Fatalf("admission = %+v: unverified, so the executor gate needs an operator", a)
	}
}

func TestOptimizer_ColdStartWithoutSnapshotsCheck(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	if o.ColdStart(context.Background()) {
		t.Fatal("min_snapshots 0 disables the cold start")
	}
}

func TestTableContext_FromTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("no test PostgreSQL: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS admit_ctx CASCADE;
		CREATE SCHEMA admit_ctx;
		CREATE TABLE admit_ctx.orders (id bigint PRIMARY KEY, customer_id bigint, status text);
		INSERT INTO admit_ctx.orders SELECT g, g % 50, 'open' FROM generate_series(1, 500) g;
		ANALYZE admit_ctx.orders;`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS admit_ctx CASCADE") })
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Query: "SELECT * FROM admit_ctx.orders WHERE customer_id = $1",
				Calls: 50, TotalExecTime: 500, MeanExecTime: 10},
			{QueryID: 2, Query: "SELECT * FROM admit_ctx.other WHERE x = $1", Calls: 50,
				TotalExecTime: 500, MeanExecTime: 10},
		},
		Tables: []collector.TableStats{
			{SchemaName: "admit_ctx", RelName: "orders", NLiveTup: 500},
			{SchemaName: "admit_ctx", RelName: "quiet", NLiveTup: 5},
		},
		Indexes: []collector.IndexStats{{SchemaName: "admit_ctx", RelName: "orders",
			IndexRelName: "orders_pkey", IsUnique: true, IsValid: true,
			IndexDef: "CREATE UNIQUE INDEX orders_pkey ON admit_ctx.orders USING btree (id)"}},
	}
	o := New(pool, admissionConfig(), admitNoLog)
	tc, ok, err := o.TableContext(ctx, snap, "admit_ctx.orders")
	if err != nil || !ok {
		t.Fatalf("context: ok %v err %v", ok, err)
	}
	if len(tc.Queries) != 1 || tc.Queries[0].QueryID != 1 || len(tc.Indexes) != 1 {
		t.Fatalf("only the table's own statements and indexes: %+v", tc)
	}
	var cols []string
	for _, c := range tc.Columns {
		cols = append(cols, c.Name)
	}
	if strings.Join(cols, ",") != "id,customer_id,status" {
		t.Fatalf("columns = %v", cols)
	}
	if tc, ok, err := o.TableContext(ctx, snap, "admit_ctx.quiet"); err != nil || !ok ||
		len(tc.Queries) != 0 {
		t.Fatalf("a table without statements still has a context: %+v %v %v", tc, ok, err)
	}
	if _, ok, err := o.TableContext(ctx, snap, "admit_ctx.missing"); err != nil || ok {
		t.Fatalf("a table outside the snapshot: ok %v err %v", ok, err)
	}
}

// HypoPG cannot model every access method (indexes-06): a GIN, GiST, HNSW
// or IVFFlat candidate is unverified with the method named, without a
// hypothetical create (which would fail as "what-if evaluation failed").

func ginTable() TableContext {
	tc := admitTable()
	tc.Columns = append(tc.Columns, ColumnInfo{Name: "payload", Type: "jsonb"})
	return tc
}

const ginDDL = "CREATE INDEX CONCURRENTLY ai_claims_payload_idx ON public.ai_claims " +
	"USING gin (payload)"

func TestAdmit_GINGetsTheMethodReasonWithoutAWhatIf(t *testing.T) {
	o, w, logs := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
		Improvement: 50, SizeBytes: 8192})
	a := o.Admit(context.Background(), candidate(ginDDL), ginTable())
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfUnverified || a.Rec.Validated {
		t.Fatalf("admission = %+v: a GIN index is unverified, approval required", a)
	}
	want := "HypoPG cannot model gin indexes; operator approval required"
	if a.Rec.WhatIfReason != want {
		t.Fatalf("reason = %q, want %q", a.Rec.WhatIfReason, want)
	}
	if w.calls.Load() != 0 {
		t.Fatalf("what-ifs = %d: a method HypoPG cannot model is never created", w.calls.Load())
	}
	if got := logs.matching("hypopg validation failed"); len(got) != 0 {
		t.Fatalf("an unsupported method was logged as a failure: %q", got)
	}
}

func TestAdmit_UnsupportedMethodOutranksUnavailableHypoPG(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	o.whatIf = unavailableWhatIf{}
	a := o.Admit(context.Background(), candidate(ginDDL), ginTable())
	if !strings.HasPrefix(a.Rec.WhatIfReason, "HypoPG cannot model gin indexes") {
		t.Fatalf("reason = %q: installing HypoPG would not help, so name the method",
			a.Rec.WhatIfReason)
	}
}

func TestEnrich_MethodsHypoPGCannotModel(t *testing.T) {
	cases := map[string]string{
		"gist": "HypoPG cannot model gist indexes; operator approval required",
		"hnsw": "HypoPG cannot model hnsw indexes; operator approval required; " +
			"measure it with pg_sage vector-lab",
		"ivfflat": "HypoPG cannot model ivfflat indexes; operator approval required; " +
			"measure it with pg_sage vector-lab",
		"spgist": "HypoPG cannot model spgist indexes; operator approval required",
	}
	for method, want := range cases {
		o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
			Improvement: 50, SizeBytes: 8192})
		rec := candidate("CREATE INDEX CONCURRENTLY x ON public.ai_claims USING " +
			strings.ToUpper(method) + " (status)")
		got, rejected := o.enrichWithHypoPG(context.Background(), rec, admitTable())
		if rejected || got.WhatIf != WhatIfUnverified || got.WhatIfReason != want ||
			got.Validated || w.calls.Load() != 0 {
			t.Fatalf("%s: rejected=%t rec=%+v what-ifs=%d", method, rejected, got,
				w.calls.Load())
		}
	}
}

func TestEnrich_MethodsHypoPGModelsReachTheWhatIf(t *testing.T) {
	for _, using := range []string{"", "USING btree", "USING brin", "USING hash",
		"USING bloom"} {
		o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
			Improvement: 50, SizeBytes: 8192})
		rec := candidate("CREATE INDEX CONCURRENTLY x ON public.ai_claims " + using +
			" (status)")
		got, _ := o.enrichWithHypoPG(context.Background(), rec, admitTable())
		if w.calls.Load() != 1 || got.WhatIf != WhatIfVerified {
			t.Fatalf("%q: what-ifs %d, verdict %q (%s)", using, w.calls.Load(), got.WhatIf,
				got.WhatIfReason)
		}
	}
}

// A partial measurement stays unverified whatever the planned statements
// show (an unplanned statement's regression is unknown), but the reason
// says what was measured and which statements were not.
func TestAdmit_PartialMeasurementNamesTheUnplannedStatements(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 4, Failed: 1,
		Improvement: 62, SizeBytes: 8192, UnplannedQueryIDs: []int64{123}})
	a := o.Admit(context.Background(), candidate(statusDDL), admitTable())
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfUnverified || a.Rec.Validated ||
		w.calls.Load() != 1 {
		t.Fatalf("admission = %+v: a partial measurement is unverified", a)
	}
	want := "planned statements improved 62.0% (call-weighted, 4 of 5); " +
		"not planned: queryid 123"
	if a.Rec.WhatIfReason != want {
		t.Fatalf("reason = %q, want %q", a.Rec.WhatIfReason, want)
	}
	if a.Rec.WhatIfMeasured != 4 || len(a.Rec.WhatIfUnplannedQueryIDs) != 1 ||
		a.Rec.WhatIfUnplannedQueryIDs[0] != 123 {
		t.Fatalf("evidence: measured %d, unplanned %v", a.Rec.WhatIfMeasured,
			a.Rec.WhatIfUnplannedQueryIDs)
	}
}
