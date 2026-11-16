package optimizer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04a: the index floor rehearses each admitted candidate on a clone
// when a rehearser is configured, and queues it either way with the
// tier-2 record (or the reason there is none) on its ladder.

type fakeIndexRehearser struct {
	mu      sync.Mutex
	ddls    []string
	queries [][]QueryInfo
	rec     func(n int) verify.LadderRecord
}

func (f *fakeIndexRehearser) RehearseIndex(_ context.Context, ddl string,
	qs []QueryInfo) verify.LadderRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ddls = append(f.ddls, ddl)
	f.queries = append(f.queries, qs)
	return f.rec(len(f.ddls))
}

func passedClone(int) verify.LadderRecord {
	pct := -60.0
	return verify.FromRehearsal(verify.Rehearsal{Passed: true, BlocksPerCallPct: &pct,
		ReceiptID: 12, Provider: "dle"}, time.Now())
}

func rehearseFloor(t *testing.T, reh IndexRehearser, stmts ...string) (FloorResult,
	*fakeFloorOpt) {
	t.Helper()
	opt := &fakeFloorOpt{contexts: map[string]TableContext{
		"public.orders": floorTC("orders", stmts...)}}
	logs := &floorLogs{}
	f := (&Floor{opt: opt, logFn: logs.fn}).WithRehearser(reh)
	return f.Run(context.Background(), nil, []string{"public.orders"}, nil), opt
}

func TestFloorRehearse_NoProviderQueuesWithTheSkip(t *testing.T) {
	res, _ := rehearseFloor(t, nil,
		"SELECT id FROM public.orders WHERE customer_id = $1")
	if len(res.Admitted) != 1 {
		t.Fatalf("admitted %+v", res.Admitted)
	}
	ladder := res.Admitted[0].Rec.Ladder
	if len(ladder) != 1 || ladder[0].Tier != verify.TierClone ||
		ladder[0].Result != verify.LadderSkipped ||
		ladder[0].Skipped[0].Reason != "no clone provider" {
		t.Fatalf("ladder = %+v", ladder)
	}
}

func TestFloorRehearse_RehearsesEachAdmittedCandidate(t *testing.T) {
	reh := &fakeIndexRehearser{rec: passedClone}
	res, _ := rehearseFloor(t, reh,
		"SELECT id FROM public.orders WHERE customer_id = $1",
		"SELECT id FROM public.orders WHERE region = $1")
	if len(res.Admitted) != 2 || len(reh.ddls) != 2 {
		t.Fatalf("admitted %d rehearsed %d", len(res.Admitted), len(reh.ddls))
	}
	for i, adm := range res.Admitted {
		if reh.ddls[i] != adm.Rec.DDL {
			t.Fatalf("rehearsed %q for candidate %q", reh.ddls[i], adm.Rec.DDL)
		}
		if len(adm.Rec.Ladder) != 1 || !adm.Rec.Ladder[0].Passed() ||
			adm.Rec.Ladder[0].Evidence["receipt_id"] != int64(12) {
			t.Fatalf("candidate %d ladder %+v", i, adm.Rec.Ladder)
		}
	}
	if len(reh.queries[0]) != 2 || reh.queries[0][0].QueryID != 1 {
		t.Fatalf("the table's statements are the targets: %+v", reh.queries[0])
	}
}

func TestFloorRehearse_FailureStillQueues(t *testing.T) {
	reh := &fakeIndexRehearser{rec: func(int) verify.LadderRecord {
		return verify.LadderBuildError(verify.TierClone, verify.MethodLadderRehearsal,
			errors.New("tuning: no clone is available: dle: 503"), time.Now())
	}}
	res, _ := rehearseFloor(t, reh, "SELECT id FROM public.orders WHERE customer_id = $1")
	if len(res.Admitted) != 1 {
		t.Fatalf("a failed rehearsal still proposes (recommend-only unchanged): %+v",
			res.Admitted)
	}
	rec := res.Admitted[0].Rec.Ladder[0]
	if rec.Passed() || !strings.Contains(rec.Skipped[0].Reason, "dle: 503") {
		t.Fatalf("ladder %+v", rec)
	}
}

// No DB: target selection is pure.
func TestFloorTargets_Bounds(t *testing.T) {
	var stmts []string
	for i := 0; i < 8; i++ {
		stmts = append(stmts, "SELECT id FROM public.orders WHERE customer_id = $1")
	}
	tc := floorTC("orders", stmts...)
	if got := floorTargets(Recommendation{}, tc); len(got) != floorRehearseMax {
		t.Fatalf("at most %d targets, got %d", floorRehearseMax, len(got))
	}
	got := floorTargets(Recommendation{AffectedQueryIDs: []int64{7, 3}}, tc)
	if len(got) != 2 || got[0].QueryID != 3 || got[1].QueryID != 7 {
		t.Fatalf("named targets only, in context order: %+v", got)
	}
	if got := floorTargets(Recommendation{AffectedQueryIDs: []int64{99}}, tc); len(got) != 0 {
		t.Fatalf("no matching target: %+v", got)
	}
	tc.Queries = nil
	if got := floorTargets(Recommendation{}, tc); len(got) != 0 {
		t.Fatalf("no statements: %+v", got)
	}
}

func TestFloorRehearse_NoTargetsIsASkip(t *testing.T) {
	reh := &fakeIndexRehearser{rec: passedClone}
	f := (&Floor{logFn: func(string, string, ...any) {}}).WithRehearser(reh)
	adm := Admission{Rec: Recommendation{DDL: "CREATE INDEX x ON public.orders (a)",
		AffectedQueryIDs: []int64{42}}}
	f.rehearse(context.Background(), &adm, floorTC("orders"))
	if len(reh.ddls) != 0 || adm.Rec.Ladder[0].Skipped[0].Reason != floorNoTargets {
		t.Fatalf("no target: rehearsed %v ladder %+v", reh.ddls, adm.Rec.Ladder)
	}
	var nilFloor *Floor
	if nilFloor.WithRehearser(reh) != nil {
		t.Fatal("a nil floor stays nil")
	}
}

// No concurrent access test here: Floor.Run is called by one analyzer
// cycle at a time and holds no state between runs; the shared cap is
// tuning's LimitRehearsals, tested there.
