package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04b: the rehearsal names its clone receipt and becomes a tier-2
// ladder record.

// receiptProvider hands out a new clone per Create and, like
// clone.ReceiptingProvider, names a receipt only while the clone lives.
type receiptProvider struct {
	mu      sync.Mutex
	next    int
	live    map[string]int64
	destroy error
}

func (p *receiptProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return time.Minute, nil
}

func (p *receiptProvider) Create(context.Context, clonepkg.CloneSpec) (clonepkg.Clone, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := fmt.Sprintf("clone-%d", p.next)
	if p.live == nil {
		p.live = map[string]int64{}
	}
	p.live[id] = int64(100 + p.next)
	return clonepkg.Clone{ID: id, DSN: "postgres://" + id + ".invalid/app"}, nil
}

func (p *receiptProvider) Destroy(_ context.Context, c clonepkg.Clone) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, c.ID)
	return p.destroy
}

func (p *receiptProvider) ReceiptOf(id string) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.live[id]
	return r, ok
}

// dsnRunner records the DSN it measured on and returns a measured
// workload whose query id is derived from the DSN.
type dsnRunner struct {
	mu   sync.Mutex
	dsns []string
}

func (r *dsnRunner) Run(_ context.Context, dsn string, _ planpkg.Plan) (Measurement, error) {
	r.mu.Lock()
	r.dsns = append(r.dsns, dsn)
	r.mu.Unlock()
	return Measurement{AffectedQueries: []QueryMeasurement{{QueryID: int64(len(dsn)),
		BeforeLatencyMS: 2, AfterLatencyMS: 2, BeforePlanHash: dsn, AfterPlanHash: dsn}},
	}, nil
}

// Happy path: the receipt is named while the clone lives (the provider
// forgets it on destroy) and the provider and time are on the result.
func TestRehearseNamesTheReceiptBeforeDestroy(t *testing.T) {
	provider := &receiptProvider{}
	options := testOptions()
	options.Provider = "dle"
	before := time.Now()
	got, err := NewOrchestrator(provider, &dsnRunner{}, options).Rehearse(
		context.Background(), rehearsalPlan())
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if got.CloneID != "clone-1" || got.ReceiptID != 101 || got.Provider != "dle" ||
		got.At.Before(before) {
		t.Fatalf("result = %#v", got)
	}
	if len(provider.live) != 0 {
		t.Fatalf("clone not destroyed: %v", provider.live)
	}
}

// Concurrency: two rehearsals at once get different clones and receipts,
// and each measures on its own clone.
func TestConcurrentRehearsalsMeasureOnTheirOwnClones(t *testing.T) {
	provider := &receiptProvider{}
	runner := &dsnRunner{}
	orchestrator := NewOrchestrator(provider, runner, testOptions())
	results := make([]Result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := orchestrator.Rehearse(context.Background(), rehearsalPlan())
			if err != nil {
				t.Errorf("Rehearse %d: %v", i, err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	a, b := results[0], results[1]
	if a.CloneID == b.CloneID || a.ReceiptID == b.ReceiptID || a.ReceiptID == 0 {
		t.Fatalf("clones %q/%q receipts %d/%d", a.CloneID, b.CloneID, a.ReceiptID,
			b.ReceiptID)
	}
	for _, res := range results {
		hash := res.Measurement.AffectedQueries[0].BeforePlanHash
		if !strings.Contains(hash, res.CloneID+".invalid") {
			t.Fatalf("clone %s measured on %s", res.CloneID, hash)
		}
	}
	if len(runner.dsns) != 2 || runner.dsns[0] == runner.dsns[1] {
		t.Fatalf("runner DSNs = %v", runner.dsns)
	}
}

// The verdicts map onto the ladder: promote passes, park fails,
// recommend-only is inconclusive; each carries the receipt and counts.
func TestLadderRecordOfEachVerdict(t *testing.T) {
	measured := Measurement{AffectedQueries: []QueryMeasurement{
		{QueryID: 1, BeforeCost: 5, AfterCost: 5, BeforePlanHash: "a", AfterPlanHash: "a",
			NotExecuted: "not executed: parameters unknown"},
		{QueryID: 2, Unmeasured: UnmeasuredNotOne},
	}}
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		verdict Verdict
		reason  Reason
		want    string
	}{
		{VerdictPromoteExpand, ReasonRehearsalPassed, verify.LadderPassed},
		{VerdictPark, ReasonPlanRegression, verify.LadderFailed},
		{VerdictRecommendOnly, ReasonInconclusiveWorkload, verify.LadderInconclusive},
	} {
		rec := Ladder(Result{Verdict: test.verdict, Reason: test.reason, Detail: "d",
			Measurement: measured, CloneID: "clone-1", ReceiptID: 77, Provider: "dle",
			At: at})
		if rec.Tier != verify.TierClone || rec.Method != verify.MethodLadderRehearsal ||
			rec.Result != test.want || !rec.At.Equal(at) {
			t.Fatalf("%s: record = %#v", test.verdict, rec)
		}
		if rec.Evidence["receipt_id"] != int64(77) || rec.Evidence["provider"] != "dle" ||
			rec.Evidence["measured"] != 1 || rec.Evidence["statements"] != 2 ||
			rec.Evidence["reason"] != string(test.reason)+": d" {
			t.Fatalf("%s: evidence = %#v", test.verdict, rec.Evidence)
		}
		if rec.Passed() != (test.want == verify.LadderPassed) {
			t.Fatalf("%s: Passed() = %v", test.verdict, rec.Passed())
		}
	}
}

// Nil/empty: no clone is a skipped tier with the reason; a leaked clone
// never passes.
func TestLadderRecordWithoutCloneOrWithLeak(t *testing.T) {
	rec := Ladder(Result{Verdict: VerdictRecommendOnly, Reason: ReasonStaleClone})
	if rec.Result != verify.LadderSkipped || len(rec.Skipped) != 1 ||
		rec.Skipped[0].Reason != string(ReasonStaleClone) || rec.Passed() {
		t.Fatalf("stale record = %#v", rec)
	}
	if rec := Ladder(Result{}); rec.Skipped[0].Reason != "no rehearsal clone" {
		t.Fatalf("empty record = %#v", rec)
	}
	leak := Ladder(Result{Verdict: VerdictPromoteExpand, Reason: ReasonRehearsalPassed,
		CloneID: "clone-1", CleanupError: "destroy rehearsal clone clone-1: busy"})
	if leak.Passed() || leak.Evidence["cleanup_error"] == nil {
		t.Fatalf("leak record = %#v", leak)
	}
}

// Error propagation: a runner failure after the clone exists still names
// the clone and its receipt, so the ladder links to it.
func TestRehearseFailureKeepsTheReceipt(t *testing.T) {
	provider := &receiptProvider{}
	runner := &fakeRunner{err: errors.New("lock timeout")}
	got, err := NewOrchestrator(provider, runner, testOptions()).Rehearse(
		context.Background(), rehearsalPlan())
	if !errors.Is(err, ErrStepFailed) || got.ReceiptID != 101 || got.CloneID != "clone-1" {
		t.Fatalf("result = %#v err = %v", got, err)
	}
}

var _ clonepkg.ReceiptNamer = (*receiptProvider)(nil)
