package tuning

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04a: a rehearsal's tier-2 record, and the per-hour cap.

func TestRehearsalLadder_Results(t *testing.T) {
	for name, tc := range map[string]struct {
		qs       []RehearsedQuery
		result   string
		pct      *float64
		reasonIn string
	}{
		"blocks fell": {qs: []RehearsedQuery{{BeforeBlocks: blocks(400),
			AfterBlocks: blocks(100)}, {NotExecuted: notExecutedParams}},
			result: verify.LadderPassed, pct: pct(-75)},
		"blocks equal": {qs: []RehearsedQuery{{BeforeBlocks: blocks(50),
			AfterBlocks: blocks(50)}}, result: verify.LadderFailed, pct: pct(0)},
		"blocks rose": {qs: []RehearsedQuery{{BeforeBlocks: blocks(50),
			AfterBlocks: blocks(75)}}, result: verify.LadderFailed, pct: pct(50)},
		"zero before": {qs: []RehearsedQuery{{BeforeBlocks: blocks(0),
			AfterBlocks: blocks(0)}}, result: verify.LadderFailed, pct: pct(0)},
		"only planned": {qs: []RehearsedQuery{{NotExecuted: notExecutedParams}},
			result: verify.LadderInconclusive, reasonIn: "parameters unknown"},
		"after failed": {qs: []RehearsedQuery{{BeforeBlocks: blocks(9),
			Error: "timed out on the clone"}}, result: verify.LadderInconclusive,
			reasonIn: "timed out"},
		"no queries": {result: verify.LadderInconclusive, reasonIn: "no statement"},
	} {
		rec := RehearsalLadder(RehearsalResult{Queries: tc.qs, ReceiptID: 5}, nil, t0)
		if rec.Result != tc.result || rec.Tier != verify.TierClone ||
			rec.Evidence["receipt_id"] != int64(5) {
			t.Errorf("%s: %+v", name, rec)
			continue
		}
		if tc.pct != nil && (rec.Predicted == nil || *rec.Predicted != *tc.pct ||
			rec.Metric != verify.LadderMetricBlocksPerCall) {
			t.Errorf("%s: predicted %v metric %q", name, rec.Predicted, rec.Metric)
		}
		if tc.reasonIn != "" && (len(rec.Skipped) == 0 ||
			!strings.Contains(rec.Skipped[0].Reason, tc.reasonIn)) {
			t.Errorf("%s: skipped %+v", name, rec.Skipped)
		}
	}
}

func TestRehearsalLadder_ErrorIsSkippedNeverPassed(t *testing.T) {
	rec := RehearsalLadder(RehearsalResult{ReceiptID: 9, Queries: []RehearsedQuery{{
		BeforeBlocks: blocks(10), AfterBlocks: blocks(1)}}}, errors.New("destroy failed"), t0)
	if rec.Passed() || rec.Result != verify.LadderSkipped ||
		rec.Evidence["receipt_id"] != int64(9) ||
		rec.Skipped[0].Reason != "destroy failed" {
		t.Fatalf("an error with measurements is still a skip: %+v", rec)
	}
}

type countingRehearser struct {
	n   atomic.Int64
	err error
}

func (c *countingRehearser) Rehearse(context.Context, RehearsalRequest) (RehearsalResult,
	error) {
	c.n.Add(1)
	return RehearsalResult{BuildMs: 1}, c.err
}

func TestLimitRehearsals_ThirdInAnHourIsSkipped(t *testing.T) {
	now := t0
	inner := &countingRehearser{}
	l := LimitRehearsals(inner, 0, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if _, err := l.Rehearse(context.Background(), rehearsalRequest()); err != nil {
			t.Fatalf("rehearsal %d: %v", i+1, err)
		}
		now = now.Add(20 * time.Minute)
	}
	_, err := l.Rehearse(context.Background(), rehearsalRequest())
	if !errors.Is(err, ErrRehearsalCapped) || !strings.Contains(err.Error(),
		"the cap is 2 per hour") || inner.n.Load() != 2 {
		t.Fatalf("third within the hour: %v (inner %d)", err, inner.n.Load())
	}
	rec := RehearsalLadder(RehearsalResult{}, err, now)
	if rec.Result != verify.LadderSkipped || !strings.Contains(rec.Skipped[0].Reason, "cap") {
		t.Fatalf("the cap is a skipped tier with the reason: %+v", rec)
	}
	now = t0.Add(time.Hour) // the first rehearsal is exactly an hour old
	if _, err := l.Rehearse(context.Background(), rehearsalRequest()); err != nil {
		t.Fatalf("an hour after the first: %v", err)
	}
}

func TestLimitRehearsals_RefusedRequestsDoNotUseTheCap(t *testing.T) {
	inner := &countingRehearser{err: ErrRehearsalRefused}
	l := LimitRehearsals(inner, 1, func() time.Time { return t0 })
	for i := 0; i < 3; i++ {
		if _, err := l.Rehearse(context.Background(), rehearsalRequest()); !errors.Is(err,
			ErrRehearsalRefused) {
			t.Fatalf("refusal %d: %v", i, err)
		}
	}
	inner.err = ErrCloneUnavailable // a clone attempt uses the cap
	_, _ = l.Rehearse(context.Background(), rehearsalRequest())
	if _, err := l.Rehearse(context.Background(), rehearsalRequest()); !errors.Is(err,
		ErrRehearsalCapped) {
		t.Fatalf("a failed clone attempt counts: %v", err)
	}
}

func TestLimitRehearsals_ConcurrentCallersShareTheCap(t *testing.T) {
	inner := &countingRehearser{}
	l := LimitRehearsals(inner, 3, func() time.Time { return t0 })
	var wg sync.WaitGroup
	var capped atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Rehearse(context.Background(), rehearsalRequest()); errors.Is(err,
				ErrRehearsalCapped) {
				capped.Add(1)
			}
		}()
	}
	wg.Wait()
	if inner.n.Load() != 3 || capped.Load() != 17 {
		t.Fatalf("ran %d capped %d, want 3 and 17", inner.n.Load(), capped.Load())
	}
}
