package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The optimizer consults rejection memory for LLM candidates: a candidate
// that repeats a measured, rejected idea on an unchanged workload skips the
// what-if; the model is told which shapes were already measured. The LLM is
// a fake OpenAI-compatible server; the what-if is a counting fake.

// scriptedModel is a fake OpenAI-compatible endpoint that answers with the
// scripted replies in order (the last one repeats) and records each user
// prompt it received.
type scriptedModel struct {
	mu      sync.Mutex
	replies []string
	status  int
	prompts []string
	srv     *httptest.Server
}

func newScriptedModel(t *testing.T, replies ...string) *scriptedModel {
	t.Helper()
	m := &scriptedModel{replies: replies}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	for _, msg := range body.Messages {
		if msg.Role == "user" {
			m.prompts = append(m.prompts, msg.Content)
		}
	}
	reply := m.replies[min(len(m.prompts), len(m.replies))-1]
	status := m.status
	m.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Write(fnTestChatJSON(reply, 40))
}

func (m *scriptedModel) prompt(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.prompts) {
		return ""
	}
	return m.prompts[i]
}

// countWhatIf counts evaluations; safe for concurrent cycles.
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

func recReply(ddls ...string) string {
	recs := make([]Recommendation, len(ddls))
	for i, ddl := range ddls {
		recs[i] = Recommendation{Table: "public.ai_claims", DDL: ddl, IndexType: "btree",
			Rationale: "prefix lookups", Severity: "info", Category: "missing_index"}
	}
	return fnTestRecJSON(recs)
}

func claimsTable() TableContext {
	tc := memTable()
	tc.Columns = []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "status", Type: "text"},
		{Name: "evidence_event_ids_json", Type: "text"}}
	tc.Queries[0].Text = "SELECT id FROM ai_claims WHERE evidence_event_ids_json LIKE $1"
	tc.Queries[1].Text = "SELECT status FROM ai_claims WHERE id = $1"
	tc.WriteRate, tc.IndexCount, tc.Workload = 5, 1, "oltp_read"
	return tc
}

func memOptimizer(t *testing.T, model *scriptedModel, store *memStore,
	result WhatIfResult) (*Optimizer, *countWhatIf, *logRecorder) {
	t.Helper()
	llmCfg := fnTestLLMConfig(model.srv.URL)
	llmCfg.CooldownSeconds = 0 // concurrent cycles send identical prompts
	logs := &logRecorder{}
	o := New(llm.New(llmCfg, fnNoopLog), nil, nil, fnTestOptimizerConfig(), 160000, 8192,
		logs.log)
	w := &countWhatIf{result: result}
	o.whatIf = w
	o.memory = testMemory(store, logs)
	return o, w, logs
}

type cycleOutcome struct {
	recs       []Recommendation
	rejections int
	skipped    int
	err        error
}

func runMemCycle(ctx context.Context, o *Optimizer, tc TableContext) cycleOutcome {
	v := o.memory.view(ctx, tc)
	recs, _, rej, err := o.analyzeTable(ctx, tc, v)
	return cycleOutcome{recs: recs, rejections: rej, skipped: v.skipped, err: err}
}

// The lifeos replay: three cycles, three names and INCLUDE lists for one
// idea. Only the first is measured; the others are skipped, and the model
// is told after the first cycle.
func TestRejectionMemory_LifeosReplayMeasuresOnce(t *testing.T) {
	model := newScriptedModel(t,
		recReply(lifeosDDL("ai_claims_evidence_pattern_idx", "id, status")),
		recReply(lifeosDDL("ai_claims_evidence_event_ids_pattern_idx", "id")),
		recReply(lifeosDDL("ai_claims_evidence_prefix_idx", "status, id")))
	store := newMemStore()
	o, w, _ := memOptimizer(t, model, store, zeroGain)
	ctx := context.Background()
	for cycle := 0; cycle < 3; cycle++ {
		out := runMemCycle(ctx, o, claimsTable())
		if out.err != nil || len(out.recs) != 0 || out.rejections != 1 {
			t.Fatalf("cycle %d: %+v", cycle, out)
		}
		if wantSkip := min(cycle, 1); out.skipped != wantSkip {
			t.Fatalf("cycle %d skipped %d, want %d", cycle, out.skipped, wantSkip)
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
	shape := "btree (evidence_event_ids_json text_pattern_ops) INCLUDE (id, status)"
	if strings.Contains(model.prompt(0), "Already measured") {
		t.Fatal("the first prompt claims a measurement that did not happen yet")
	}
	for i := 1; i < 3; i++ {
		p := model.prompt(i)
		if !strings.Contains(p, "Already measured") || !strings.Contains(p, shape) ||
			!strings.Contains(p, "0.0%") {
			t.Fatalf("prompt %d does not feed back the measured shape:\n%s", i, p)
		}
	}
}

func TestRejectionMemory_LLMOutputVariants(t *testing.T) {
	variant := lifeosDDL("ai_claims_evidence_prefix_idx", "id")
	cases := []struct {
		name, reply string
		wantErr     error
		skipped     int
	}{
		{"json fences", "```json\n" + recReply(variant) + "\n```", nil, 1},
		{"thinking prefix", "Considering the table...\n" + recReply(variant), nil, 1},
		{"empty array", "[]", nil, 0},
		{"malformed json", `[{"ddl": "CREATE INDEX`, errors.New("parse"), 0},
		{"empty reply", "   ", llm.ErrEmptyResponse, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stored := storedRejection(t, lifeosDDL("ai_claims_evidence_pattern_idx",
				"id, status"), time.Hour)
			store := newMemStore(stored)
			o, w, _ := memOptimizer(t, newScriptedModel(t, c.reply), store, zeroGain)
			out := runMemCycle(context.Background(), o, claimsTable())
			switch {
			case c.wantErr == nil && out.err != nil:
				t.Fatalf("unexpected error: %v", out.err)
			case c.wantErr == llm.ErrEmptyResponse && !errors.Is(out.err, llm.ErrEmptyResponse):
				t.Fatalf("empty reply error = %v, want ErrEmptyResponse", out.err)
			case c.wantErr != nil && (out.err == nil ||
				!strings.Contains(out.err.Error(), c.wantErr.Error()) &&
					!errors.Is(out.err, c.wantErr)):
				t.Fatalf("error = %v, want %v", out.err, c.wantErr)
			}
			if out.skipped != c.skipped || w.calls.Load() != 0 || store.records != 0 {
				t.Fatalf("skipped=%d whatif=%d records=%d", out.skipped, w.calls.Load(),
					store.records)
			}
		})
	}
}

// A rate-limited model that never answers within the cycle's deadline
// writes nothing to memory and runs no what-if.
func TestRejectionMemory_RateLimitedModelLeavesMemoryAlone(t *testing.T) {
	model := newScriptedModel(t, "[]")
	model.status = http.StatusTooManyRequests
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id"), time.Hour))
	o, w, _ := memOptimizer(t, model, store, zeroGain)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out := runMemCycle(ctx, o, claimsTable())
	if out.err == nil || !strings.Contains(out.err.Error(), "llm chat") {
		t.Fatalf("429 until the deadline must fail the table: %v", out.err)
	}
	if w.calls.Load() != 0 || store.records != 0 || store.snapshot()[0].MeasureCount != 1 {
		t.Fatalf("whatif=%d records=%d", w.calls.Load(), store.records)
	}
}

func TestRejectionMemory_MaterialChangeReevaluates(t *testing.T) {
	model := newScriptedModel(t, recReply(lifeosDDL("a", "id, status")),
		recReply(lifeosDDL("b", "id")))
	store := newMemStore()
	o, w, _ := memOptimizer(t, model, store, zeroGain)
	ctx := context.Background()
	if out := runMemCycle(ctx, o, claimsTable()); out.err != nil || out.rejections != 1 {
		t.Fatalf("cycle 1: %+v", out)
	}
	busier := claimsTable()
	busier.Queries[0].Calls *= 3
	out := runMemCycle(ctx, o, busier)
	if out.err != nil || out.skipped != 0 || w.calls.Load() != 2 {
		t.Fatalf("a 3x call volume must re-measure: %+v whatif=%d", out, w.calls.Load())
	}
	if strings.Contains(model.prompt(1), "Already measured") {
		t.Fatal("a rejection measured on another workload was fed to the model")
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
	o, w, _ := memOptimizer(t, newScriptedModel(t, recReply(other)), store, zeroGain)
	out := runMemCycle(context.Background(), o, claimsTable())
	if out.err != nil || out.skipped != 0 || w.calls.Load() != 1 || len(store.snapshot()) != 2 {
		t.Fatalf("different opclass: %+v whatif=%d rows=%d", out, w.calls.Load(),
			len(store.snapshot()))
	}
}

func TestRejectionMemory_SameIdeaTwiceInOneReply(t *testing.T) {
	model := newScriptedModel(t, recReply(lifeosDDL("a", "id, status"), lifeosDDL("b", "id")))
	o, w, _ := memOptimizer(t, model, newMemStore(), zeroGain)
	out := runMemCycle(context.Background(), o, claimsTable())
	if out.err != nil || out.rejections != 2 || out.skipped != 1 || w.calls.Load() != 1 {
		t.Fatalf("second copy must be skipped in the same cycle: %+v whatif=%d", out,
			w.calls.Load())
	}
}

func TestRejectionMemory_OnlyCompleteRejectionsAreRemembered(t *testing.T) {
	for name, res := range map[string]WhatIfResult{
		"verified gain":   {Measured: 2, Improvement: 55, SizeBytes: 8192},
		"partly planned":  {Measured: 1, Failed: 1, Improvement: 0, SizeBytes: 8192},
		"nothing planned": {},
	} {
		store := newMemStore()
		o, w, _ := memOptimizer(t, newScriptedModel(t, recReply(lifeosDDL("a", "id"))),
			store, res)
		out := runMemCycle(context.Background(), o, claimsTable())
		if out.err != nil || w.calls.Load() != 1 || store.records != 0 {
			t.Errorf("%s: %+v whatif=%d records=%d", name, out, w.calls.Load(), store.records)
		}
	}
}

func TestRejectionMemory_LoadFailureStillEvaluates(t *testing.T) {
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id"), time.Hour))
	store.loadErr = errors.New("relation \"sage.optimizer_rejection\" does not exist")
	o, w, logs := memOptimizer(t, newScriptedModel(t, recReply(lifeosDDL("b", "id"))),
		store, zeroGain)
	out := runMemCycle(context.Background(), o, claimsTable())
	if out.err != nil || out.skipped != 0 || w.calls.Load() != 1 {
		t.Fatalf("memory failure must not block evaluation: %+v whatif=%d", out,
			w.calls.Load())
	}
	if len(logs.matching("WARN", "does not exist")) != 1 {
		t.Fatalf("load failure not logged: %q", logs.lines)
	}
}

// Re-evaluating an already-surfaced candidate (an open finding) is never
// blocked by memory: only new LLM candidates consult it. Its rejection is
// still remembered.
func TestRejectionMemory_ReverifyIsNeverSuppressed(t *testing.T) {
	stored := storedRejection(t, lifeosDDL("a", "id, status"), time.Hour)
	store := newMemStore(stored)
	o, w, _ := memOptimizer(t, newScriptedModel(t, "[]"), store, zeroGain)
	rec, err := canonicalizeRecommendation(Recommendation{DDL: lifeosDDL("b", "id")},
		claimsTable())
	if err != nil {
		t.Fatal(err)
	}
	rec.WhatIf = WhatIfUnverified
	checked, keep := o.reverify(context.Background(), rec, claimsTable())
	if keep || w.calls.Load() != 1 || checked.WhatIf != WhatIfRejected {
		t.Fatalf("reverify: keep=%t whatif=%d verdict=%q", keep, w.calls.Load(),
			checked.WhatIf)
	}
	if store.records != 1 {
		t.Fatalf("a reverify rejection must be remembered, records=%d", store.records)
	}
}

// Two cycles racing on the same table: each may measure before the other
// records, but every measurement lands on the one row for the shape and
// no cycle fails.
func TestRejectionMemory_ConcurrentCyclesOnOneTable(t *testing.T) {
	model := newScriptedModel(t, recReply(lifeosDDL("a", "id, status")))
	store := newMemStore()
	o, w, _ := memOptimizer(t, model, store, zeroGain)
	const cycles = 8
	var wg sync.WaitGroup
	outs := make([]cycleOutcome, cycles)
	for i := range cycles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = runMemCycle(context.Background(), o, claimsTable())
		}(i)
	}
	wg.Wait()
	measured := int(w.calls.Load())
	skipped := 0
	for i, out := range outs {
		if out.err != nil || out.rejections != 1 {
			t.Fatalf("cycle %d: %+v", i, out)
		}
		skipped += out.skipped
	}
	rows := store.snapshot()
	if len(rows) != 1 || rows[0].MeasureCount != measured || measured+skipped != cycles {
		t.Fatalf("rows=%d count=%v measured=%d skipped=%d", len(rows), rows, measured, skipped)
	}
}

func TestFormatPrompt_MeasuredRejectionsSection(t *testing.T) {
	tc := claimsTable()
	if p := FormatPrompt(tc); strings.Contains(p, "Already measured") {
		t.Fatal("prompt without memory has an Already measured section")
	}
	tc.MeasuredRejections = []string{"- btree (x) INCLUDE (id): 0.0% (minimum 10.0%)"}
	p := FormatPrompt(tc)
	if !strings.Contains(p, "### Already measured") ||
		!strings.Contains(p, "- btree (x) INCLUDE (id): 0.0% (minimum 10.0%)") {
		t.Fatalf("section missing:\n%s", p)
	}
	if !strings.Contains(SystemPrompt(), "Already measured") {
		t.Fatal("the system prompt does not tell the model what the section means")
	}
}

func TestFormatPrompt_MeasuredRejectionsBounded(t *testing.T) {
	tc := claimsTable()
	for i := 0; i < 50; i++ {
		tc.MeasuredRejections = append(tc.MeasuredRejections, "- "+strings.Repeat("k", 290))
	}
	p := FormatPrompt(tc)
	start := strings.Index(p, "### Already measured")
	if start < 0 {
		t.Fatal("section missing")
	}
	section := p[start:]
	if end := strings.Index(section[3:], "###"); end >= 0 {
		section = section[:end+3]
	}
	if len(section) > maxMeasuredSectionChars+200 {
		t.Fatalf("section is %d chars, want about %d at most", len(section),
			maxMeasuredSectionChars)
	}
}

// The default config enables memory; an optimizer without a pool has no
// store and therefore no memory.
func TestNew_NoPoolNoMemory(t *testing.T) {
	cfg := fnTestOptimizerConfig()
	cfg.RejectionMemory = config.DefaultOptimizerRejectionMemory()
	o := New(nil, nil, nil, cfg, 160000, 8192, noopLog2)
	if o.memory != nil {
		t.Fatal("memory without a database pool")
	}
	if !config.DefaultConfig().LLM.Optimizer.RejectionMemory.Enabled {
		t.Fatal("rejection memory must be on by default")
	}
}
