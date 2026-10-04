package optimizer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// After skip_llm_after (default 3) consecutive wasted proposals for a table
// — every candidate of the reply was a memory hit or a fresh what-if
// rejection — and no material change since the streak began, the optimizer
// does not ask the model about the table at all until a material change or
// the max age. Operator-requested runs never skip.

// ddlWhatIf returns a per-candidate result: gain for a DDL containing a
// marker, zero gain otherwise.
type ddlWhatIf struct {
	calls  atomic.Int32
	gainOn string
}

func (d *ddlWhatIf) IsAvailable(context.Context) bool { return true }

func (d *ddlWhatIf) Validate(_ context.Context, rec Recommendation, _ []QueryInfo,
) (WhatIfResult, error) {
	d.calls.Add(1)
	if d.gainOn != "" && strings.Contains(rec.DDL, d.gainOn) {
		return WhatIfResult{Measured: 2, Improvement: 60, SizeBytes: 8192}, nil
	}
	return zeroGain, nil
}

const usefulDDL = "CREATE INDEX CONCURRENTLY ai_claims_status_useful_idx " +
	"ON public.ai_claims (status)"

func streakOptimizer(t *testing.T, replies ...string) (*Optimizer, *scriptedModel,
	*ddlWhatIf, *logRecorder) {
	t.Helper()
	model := newScriptedModel(t, replies...)
	o, _, logs := memOptimizer(t, model, newMemStore(), zeroGain)
	w := &ddlWhatIf{gainOn: "_useful_"}
	o.whatIf = w
	return o, model, w, logs
}

func askOnce(ctx context.Context, o *Optimizer, tc TableContext) *Result {
	res := &Result{}
	o.askModel(ctx, tc, res, newCycleMemory())
	return res
}

func promptCount(m *scriptedModel) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// lifeosReplies are three proposals of one rejected idea, then repeats.
func lifeosReplies() []string {
	return []string{recReply(lifeosDDL("a", "id, status")), recReply(lifeosDDL("b", "id")),
		recReply(lifeosDDL("c", "status, id"))}
}

func TestModelSkip_BoundaryAtN(t *testing.T) {
	o, model, w, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for i := 1; i <= 3; i++ { // N-1 wasted proposals still ask; the Nth is asked too
		if res := askOnce(ctx, o, claimsTable()); res.LLMCallsSkipped != 0 {
			t.Fatalf("proposal %d skipped the model with only %d wasted before it", i, i-1)
		}
		if promptCount(model) != i {
			t.Fatalf("proposal %d: model asked %d times", i, promptCount(model))
		}
	}
	res := askOnce(ctx, o, claimsTable())
	if res.LLMCallsSkipped != 1 || promptCount(model) != 3 || w.calls.Load() != 1 {
		t.Fatalf("after 3 wasted proposals: skipped=%d asked=%d whatif=%d",
			res.LLMCallsSkipped, promptCount(model), w.calls.Load())
	}
	if st := o.MemoryStats(); st.LLMCallsSkipped != 1 || st.WhatIfSkipped != 2 {
		t.Fatalf("stats = %+v, want 1 LLM call and 2 what-ifs skipped", st)
	}
}

func TestModelSkip_ConfiguredN(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	o.memory.settings.SkipLLMAfter = 1
	ctx := context.Background()
	askOnce(ctx, o, claimsTable())
	if res := askOnce(ctx, o, claimsTable()); res.LLMCallsSkipped != 1 ||
		promptCount(model) != 1 {
		t.Fatalf("N=1: second ask skipped=%d asked=%d", res.LLMCallsSkipped,
			promptCount(model))
	}
}

func TestModelSkip_UsefulProposalResetsStreak(t *testing.T) {
	replies := []string{recReply(lifeosDDL("a", "id")), recReply(lifeosDDL("b", "id")),
		recReply(usefulDDL), recReply(lifeosDDL("c", "id")), recReply(lifeosDDL("d", "id"))}
	o, model, _, _ := streakOptimizer(t, replies...)
	ctx := context.Background()
	for range 5 {
		if res := askOnce(ctx, o, claimsTable()); res.LLMCallsSkipped != 0 {
			t.Fatal("a useful proposal must reset the streak")
		}
	}
	if promptCount(model) != 5 {
		t.Fatalf("asked %d times, want 5", promptCount(model))
	}
}

// A candidate rejected for another reason (here: an unknown column) is not
// a memory hit or a what-if rejection, so it breaks the streak too.
func TestModelSkip_OtherRejectionResetsStreak(t *testing.T) {
	bad := "CREATE INDEX CONCURRENTLY x ON public.ai_claims (no_such_column)"
	replies := append(lifeosReplies()[:2], recReply(bad), recReply(lifeosDDL("z", "id")))
	o, model, _, _ := streakOptimizer(t, replies...)
	ctx := context.Background()
	for range 4 {
		askOnce(ctx, o, claimsTable())
	}
	if res := askOnce(ctx, o, claimsTable()); res.LLMCallsSkipped != 0 ||
		promptCount(model) != 5 {
		t.Fatalf("streak survived a validator rejection: skipped=%d asked=%d",
			res.LLMCallsSkipped, promptCount(model))
	}
}

// An empty reply proposes nothing: it neither extends nor breaks the
// streak.
func TestModelSkip_EmptyReplyIsNeutral(t *testing.T) {
	replies := []string{recReply(lifeosDDL("a", "id")), recReply(lifeosDDL("b", "id")), "[]",
		recReply(lifeosDDL("c", "id"))}
	o, model, _, _ := streakOptimizer(t, replies...)
	ctx := context.Background()
	for range 4 {
		askOnce(ctx, o, claimsTable())
	}
	if res := askOnce(ctx, o, claimsTable()); res.LLMCallsSkipped != 1 ||
		promptCount(model) != 4 {
		t.Fatalf("empty reply: skipped=%d asked=%d", res.LLMCallsSkipped, promptCount(model))
	}
}

func TestModelSkip_MaterialChangeResetsStreak(t *testing.T) {
	o, model, w, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	busier := claimsTable()
	busier.Queries[0].Calls *= 3
	res := askOnce(ctx, o, busier)
	if res.LLMCallsSkipped != 0 || promptCount(model) != 4 || w.calls.Load() != 2 {
		t.Fatalf("material change: skipped=%d asked=%d whatif=%d", res.LLMCallsSkipped,
			promptCount(model), w.calls.Load())
	}
	// The streak restarted on the new workload: two more wasted proposals
	// are asked, then the model is skipped again.
	for i := 0; i < 2; i++ {
		if askOnce(ctx, o, busier).LLMCallsSkipped != 0 {
			t.Fatalf("restarted streak skipped after %d proposals", i+1)
		}
	}
	if askOnce(ctx, o, busier).LLMCallsSkipped != 1 || promptCount(model) != 6 {
		t.Fatalf("restarted streak did not skip at N: asked=%d", promptCount(model))
	}
}

func TestModelSkip_MaxAgeLiftsSkip(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	o.memory.now = func() time.Time { return memNow.Add(7*24*time.Hour - time.Second) }
	if askOnce(ctx, o, claimsTable()).LLMCallsSkipped != 1 {
		t.Fatal("a streak younger than the max age must still skip")
	}
	o.memory.now = func() time.Time { return memNow.Add(7 * 24 * time.Hour) }
	if askOnce(ctx, o, claimsTable()).LLMCallsSkipped != 0 || promptCount(model) != 4 {
		t.Fatalf("a streak at the max age must ask again: asked=%d", promptCount(model))
	}
}

// An operator-requested run asks the model and measures every candidate,
// even with a full streak and a matching remembered rejection.
func TestModelSkip_OperatorRequestBypasses(t *testing.T) {
	o, model, w, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	before := w.calls.Load()
	res := askOnce(WithOperatorRequest(ctx), o, claimsTable())
	if res.LLMCallsSkipped != 0 || res.MemorySkips != 0 || promptCount(model) != 4 ||
		w.calls.Load() != before+1 {
		t.Fatalf("operator run: llmSkipped=%d whatifSkipped=%d asked=%d whatif %d->%d",
			res.LLMCallsSkipped, res.MemorySkips, promptCount(model), before, w.calls.Load())
	}
	if operatorRequested(ctx) || !operatorRequested(WithOperatorRequest(ctx)) {
		t.Fatal("the operator marker must ride on the context only")
	}
}

func TestModelSkip_NoMemoryNeverSkips(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	o.memory = nil
	ctx := context.Background()
	for range 5 {
		if askOnce(ctx, o, claimsTable()).LLMCallsSkipped != 0 {
			t.Fatal("no memory, no skip")
		}
	}
	if promptCount(model) != 5 || o.MemoryStats() != (MemoryStats{}) {
		t.Fatalf("asked %d, stats %+v", promptCount(model), o.MemoryStats())
	}
	var nilOpt *Optimizer
	if nilOpt.MemoryStats() != (MemoryStats{}) {
		t.Fatal("nil optimizer stats must be zero")
	}
}

// Streaks are per table: another table's wasted proposals never skip this
// one.
func TestModelSkip_PerTable(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	other := claimsTable()
	other.Table = "ai_claims_archive"
	if askOnce(ctx, o, other).LLMCallsSkipped != 0 || promptCount(model) != 4 {
		t.Fatal("a streak on ai_claims skipped ai_claims_archive")
	}
}

func TestModelSkip_CycleSummaryIsOneDebugLine(t *testing.T) {
	o, _, _, logs := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	logs.lines = nil
	o.analyzeTables(ctx, []TableContext{claimsTable()}, &Result{})
	got := logs.matching("DEBUG", "rejection memory")
	if len(got) != 1 || !strings.Contains(got[0], "model for 1 table(s)") ||
		!strings.Contains(got[0], "public.ai_claims") {
		t.Fatalf("want one DEBUG summary naming the skipped table, got %q", logs.lines)
	}
	for _, line := range logs.lines {
		if !strings.HasPrefix(line, "DEBUG") && strings.Contains(line, "ai_claims") {
			t.Fatalf("a model skip was logged above DEBUG: %q", line)
		}
	}
}

// Concurrent cycles on one table share the streak safely (run with -race):
// once it is full, every racing cycle skips and each skip is counted.
func TestModelSkip_ConcurrentCycles(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	for range 3 {
		askOnce(ctx, o, claimsTable())
	}
	const racers = 8
	var wg sync.WaitGroup
	var skipped atomic.Int32
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			skipped.Add(int32(askOnce(ctx, o, claimsTable()).LLMCallsSkipped))
		}()
	}
	wg.Wait()
	if skipped.Load() != racers || o.MemoryStats().LLMCallsSkipped != racers ||
		promptCount(model) != 3 {
		t.Fatalf("racers skipped %d, stats %+v, asked %d", skipped.Load(), o.MemoryStats(),
			promptCount(model))
	}
}

// A material change in the middle of a streak restarts it on the new
// workload, so earlier wasted proposals on the old workload do not count
// toward skipping the new one.
func TestModelSkip_MaterialChangeMidStreakRestartsCount(t *testing.T) {
	o, model, _, _ := streakOptimizer(t, lifeosReplies()...)
	ctx := context.Background()
	askOnce(ctx, o, claimsTable())
	askOnce(ctx, o, claimsTable())
	busier := claimsTable()
	busier.Queries[0].Calls *= 3
	for i := 1; i <= 3; i++ { // the streak restarts at 1 on busier
		if askOnce(ctx, o, busier).LLMCallsSkipped != 0 {
			t.Fatalf("skipped after %d wasted proposals on the new workload", i-1)
		}
	}
	if askOnce(ctx, o, busier).LLMCallsSkipped != 1 || promptCount(model) != 5 {
		t.Fatalf("want the skip after 3 wasted proposals on the new workload, asked %d",
			promptCount(model))
	}
}
