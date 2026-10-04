package optimizer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Admission consults rejection memory: a candidate that repeats a measured,
// rejected idea on an unchanged workload skips the what-if, and the tuning
// agent's case packet lists the shapes already measured. (These tests ran
// through the removed per-table LLM prompt before roadmap 2.2; the memory
// rules they pin are unchanged and now apply to every candidate the agent
// hands over.) The what-if is a counting fake.

// countWhatIf counts evaluations; safe for concurrent admissions.
type countWhatIf struct {
	calls  atomic.Int32
	result WhatIfResult
}

func (c *countWhatIf) IsAvailable(context.Context) bool { return true }

func (c *countWhatIf) Validate(context.Context, Recommendation, []QueryInfo,
) (WhatIfResult, error) {
	c.calls.Add(1)
	return c.result, nil
}

var zeroGain = WhatIfResult{Measured: 2, Improvement: 0, SizeBytes: 8192}

func claimsTable() TableContext {
	tc := memTable()
	tc.Columns = []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "status", Type: "text"},
		{Name: "evidence_event_ids_json", Type: "text"}}
	tc.Queries[0].Text = "SELECT id FROM ai_claims WHERE evidence_event_ids_json LIKE $1"
	tc.Queries[1].Text = "SELECT status FROM ai_claims WHERE id = $1"
	tc.WriteRate, tc.IndexCount, tc.Workload = 5, 1, "oltp_read"
	return tc
}

func memOptimizer(store *memStore, result WhatIfResult) (*Optimizer, *countWhatIf,
	*logRecorder) {
	logs := &logRecorder{}
	o := New(nil, fnTestOptimizerConfig(), 160000, logs.log)
	w := &countWhatIf{result: result}
	o.whatIf = w
	o.memory = testMemory(store, logs)
	return o, w, logs
}

func admitDDL(ctx context.Context, o *Optimizer, tc TableContext, ddl string) Admission {
	return o.Admit(ctx, Recommendation{DDL: ddl, IndexType: "btree"}, tc)
}

// The lifeos replay: three proposals, three names and INCLUDE lists for one
// idea. Only the first is measured; the others are skipped, and the
// measured shape is listed for the agent after the first.
func TestRejectionMemory_LifeosReplayMeasuresOnce(t *testing.T) {
	store := newMemStore()
	o, w, _ := memOptimizer(store, zeroGain)
	ctx := context.Background()
	if lines := o.MeasuredRejections(ctx, claimsTable()); len(lines) != 0 {
		t.Fatalf("nothing measured yet: %v", lines)
	}
	want := []AdmissionOutcome{AdmitRejected, AdmitMeasured, AdmitMeasured}
	for i, ddl := range []string{
		lifeosDDL("ai_claims_evidence_pattern_idx", "id, status"),
		lifeosDDL("ai_claims_evidence_event_ids_pattern_idx", "id"),
		lifeosDDL("ai_claims_evidence_prefix_idx", "status, id"),
	} {
		if a := admitDDL(ctx, o, claimsTable(), ddl); a.Outcome != want[i] {
			t.Fatalf("proposal %d: %+v", i, a)
		}
	}
	if got := w.calls.Load(); got != 1 {
		t.Fatalf("what-if ran %d times, want once", got)
	}
	rows := store.snapshot()
	if len(rows) != 1 || rows[0].MeasureCount != 1 || rows[0].ImprovementPct != 0 ||
		!strings.Contains(rows[0].Reason, "below the 10.0% minimum") {
		t.Fatalf("stored rejections = %+v", rows)
	}
	lines := o.MeasuredRejections(ctx, claimsTable())
	shape := "btree (evidence_event_ids_json text_pattern_ops) INCLUDE (id, status)"
	if len(lines) != 1 || !strings.Contains(lines[0], shape) ||
		!strings.Contains(lines[0], "0.0%") {
		t.Fatalf("the measured shape is listed for the agent: %v", lines)
	}
	if o.MemoryStats().WhatIfSkipped != 2 {
		t.Fatalf("skips = %+v", o.MemoryStats())
	}
}

func TestRejectionMemory_MaterialChangeReevaluates(t *testing.T) {
	store := newMemStore()
	o, w, _ := memOptimizer(store, zeroGain)
	ctx := context.Background()
	if a := admitDDL(ctx, o, claimsTable(), lifeosDDL("a", "id, status")); a.Outcome !=
		AdmitRejected {
		t.Fatalf("first: %+v", a)
	}
	busier := claimsTable()
	busier.Queries[0].Calls *= 3
	if a := admitDDL(ctx, o, busier, lifeosDDL("b", "id")); a.Outcome != AdmitRejected ||
		w.calls.Load() != 2 {
		t.Fatalf("a 3x call volume must re-measure: %+v whatif=%d", a, w.calls.Load())
	}
	if lines := o.MeasuredRejections(ctx, busier); len(lines) != 1 {
		t.Fatalf("only the shape measured on this workload is listed: %v", lines)
	}
	rows := store.snapshot()
	if len(rows) != 2 {
		// The INCLUDE sets differ, so each variant has its own row.
		t.Fatalf("stored rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Shape.String() == mustShape(t, lifeosDDL("b", "id")).String() &&
			r.Workload[0].Calls != 3000 {
			t.Fatalf("re-measurement did not record the new workload: %+v", r.Workload)
		}
	}
}

func TestRejectionMemory_DifferentIdeaIsEvaluated(t *testing.T) {
	other := "CREATE INDEX CONCURRENTLY ai_claims_evidence_vc_idx ON public.ai_claims " +
		"(evidence_event_ids_json varchar_pattern_ops) INCLUDE (id)"
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id"), time.Hour))
	o, w, _ := memOptimizer(store, zeroGain)
	a := admitDDL(context.Background(), o, claimsTable(), other)
	if a.Outcome != AdmitRejected || w.calls.Load() != 1 || len(store.snapshot()) != 2 {
		t.Fatalf("different opclass: %+v whatif=%d rows=%d", a, w.calls.Load(),
			len(store.snapshot()))
	}
}

func TestRejectionMemory_OnlyCompleteRejectionsAreRemembered(t *testing.T) {
	for name, res := range map[string]WhatIfResult{
		"verified gain":   {Measured: 2, Improvement: 55, SizeBytes: 8192},
		"partly planned":  {Measured: 1, Failed: 1, Improvement: 0, SizeBytes: 8192},
		"nothing planned": {},
	} {
		store := newMemStore()
		o, w, _ := memOptimizer(store, res)
		a := admitDDL(context.Background(), o, claimsTable(), lifeosDDL("a", "id"))
		if a.Outcome != AdmitAccepted || w.calls.Load() != 1 || store.records != 0 {
			t.Errorf("%s: %+v whatif=%d records=%d", name, a, w.calls.Load(), store.records)
		}
	}
}

func TestRejectionMemory_LoadFailureStillEvaluates(t *testing.T) {
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id"), time.Hour))
	store.loadErr = errors.New("relation \"sage.optimizer_rejection\" does not exist")
	o, w, logs := memOptimizer(store, zeroGain)
	a := admitDDL(context.Background(), o, claimsTable(), lifeosDDL("b", "id"))
	if a.Outcome != AdmitRejected || w.calls.Load() != 1 {
		t.Fatalf("memory failure must not block evaluation: %+v whatif=%d", a,
			w.calls.Load())
	}
	if len(logs.matching("WARN", "does not exist")) != 1 {
		t.Fatalf("load failure not logged: %q", logs.lines)
	}
}

// An operator's request is never suppressed by memory; its rejection is
// still remembered.
func TestRejectionMemory_OperatorRequestIsNeverSuppressed(t *testing.T) {
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id, status"), time.Hour))
	o, w, _ := memOptimizer(store, zeroGain)
	ctx := WithOperatorRequest(context.Background())
	a := admitDDL(ctx, o, claimsTable(), lifeosDDL("b", "id"))
	if a.Outcome != AdmitRejected || w.calls.Load() != 1 {
		t.Fatalf("operator request: %+v whatif=%d", a, w.calls.Load())
	}
	if store.records != 1 {
		t.Fatalf("its rejection must be remembered, records=%d", store.records)
	}
}

// Concurrent admissions of one idea: each may measure before another
// records, but every measurement lands on the one row for the shape.
func TestRejectionMemory_ConcurrentAdmissionsOnOneTable(t *testing.T) {
	store := newMemStore()
	o, w, _ := memOptimizer(store, zeroGain)
	const n = 8
	var wg sync.WaitGroup
	outs := make([]Admission, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = admitDDL(context.Background(), o, claimsTable(),
				lifeosDDL("a", "id, status"))
		}(i)
	}
	wg.Wait()
	measured, skipped := int(w.calls.Load()), 0
	for i, a := range outs {
		switch a.Outcome {
		case AdmitMeasured:
			skipped++
		case AdmitRejected:
		default:
			t.Fatalf("admission %d: %+v", i, a)
		}
	}
	rows := store.snapshot()
	if len(rows) != 1 || rows[0].MeasureCount != measured || measured+skipped != n {
		t.Fatalf("rows=%v measured=%d skipped=%d", rows, measured, skipped)
	}
}

func TestRejectionMemory_ListedShapesAreBounded(t *testing.T) {
	var rows []rejection
	for i := 0; i < 12; i++ {
		r := storedRejection(t, lifeosDDL("a", strings.Repeat("c", i+1)), time.Hour)
		rows = append(rows, r)
	}
	o, _, _ := memOptimizer(newMemStore(rows...), zeroGain)
	lines := o.MeasuredRejections(context.Background(), claimsTable())
	if len(lines) != config.DefaultOptRejectionPromptMaxShapes {
		t.Fatalf("lines = %d, want prompt_max_shapes %d", len(lines),
			config.DefaultOptRejectionPromptMaxShapes)
	}
	for _, l := range lines {
		if len(l) > maxRejectionPromptLine {
			t.Fatalf("line of %d bytes over %d", len(l), maxRejectionPromptLine)
		}
	}
}

// The default config enables memory; an optimizer without a pool has no
// store and therefore no memory.
func TestNew_NoPoolNoMemory(t *testing.T) {
	cfg := fnTestOptimizerConfig()
	cfg.RejectionMemory = config.DefaultOptimizerRejectionMemory()
	o := New(nil, cfg, 160000, noopLog2)
	if o.memory != nil {
		t.Fatal("memory without a database pool")
	}
	if !config.DefaultConfig().LLM.Optimizer.RejectionMemory.Enabled {
		t.Fatal("rejection memory must be on by default")
	}
}
