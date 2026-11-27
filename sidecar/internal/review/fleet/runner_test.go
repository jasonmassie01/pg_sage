package fleet

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The tiered pass: rollup -> triage -> fleet review -> deep scheduling,
// and Advance (reviewed members, resuming after an emergency stop).

type rig struct {
	ledger  *memLedger
	src     *mapSource
	deep    *fakeDeep
	model   *scriptModel
	stopped atomic.Bool
	clock   *clock
	runner  *Runner
}

func newRig(t *testing.T, fleet map[string]Rollup, deepMax int) *rig {
	r := &rig{ledger: newMemLedger(), src: &mapSource{rollups: fleet},
		deep: &fakeDeep{done: map[int64]int64{}}, model: namesDrops(t),
		clock: &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}}
	r.runner = &Runner{Ledger: r.ledger, Source: r.src, Deep: r.deep, Triage: r.model,
		Review: r.model, Stopped: r.stopped.Load, Now: r.clock.now,
		Settings: Settings{DeepMax: deepMax, Workers: 8}}
	return r
}

func TestRunner_ThousandMembersWithTwentyFiveOutliers(t *testing.T) {
	seeds := seeded(25)
	r := newRig(t, synth(1000, seeds...), 30)
	in, out := 1.0, 5.0
	r.runner.Settings.Price = llm.PricingFrom(map[string]config.LLMPrice{
		"haiku-class": {InputPerM: &in, OutputPerM: &out}})
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || res.Members != 1000 || res.Outliers != 25 || len(res.Scheduled) != 25 ||
		len(r.deep.scheduled) != 25 || res.Triage.Mode != ModeModel {
		t.Fatalf("run %+v (%v): want 25 deep reviews from the model's triage", res, err)
	}
	for _, d := range r.deep.scheduled {
		if d.Tokens < DeepTokensPerReview/2 || d.Period != "2026-11" || d.PassID != res.PassID {
			t.Fatalf("deep %+v lacks its tokens, period or pass", d)
		}
	}
	if st := r.ledger.states(res.PassID); st[StateDeepScheduled] != 25 ||
		st[StateRollup] != 975 {
		t.Fatalf("states %v, want 25 deep_scheduled and 975 rollup", st)
	}
	sum := r.ledger.passes[0].summary
	if sum == nil || sum.Status != PassDone || sum.Triage.Usage.PromptTokens <= 0 ||
		sum.Triage.Usage.CostUSD == nil || *sum.Triage.Usage.CostUSD >= 10 ||
		sum.Fleet.Usage.Calls != 1 || sum.DeepTokens < 25*DeepTokensPerReview-25 {
		t.Fatalf("summary %+v: want per-tier tokens and a tier-2 cost under $10", sum)
	}
}

func TestRunner_EmptyFleetGivesNoRuns(t *testing.T) {
	r := newRig(t, nil, 30)
	res, err := r.runner.Run(context.Background(), nil)
	if err != nil || res.Skipped == "" || res.PassID != 0 || len(r.ledger.passes) != 0 ||
		r.model.calls() != 0 || len(r.deep.scheduled) != 0 {
		t.Fatalf("empty fleet: %+v (%v), %d passes", res, err, len(r.ledger.passes))
	}
}

func TestRunner_FleetOfOneSkipsTheModelTiers(t *testing.T) {
	r := newRig(t, synth(1, 1), 30)
	res, err := r.runner.Run(context.Background(), []string{"db0001"})
	if err != nil || r.model.calls() != 0 || res.Fleet.Skipped == "" ||
		res.Triage.Mode != ModeDeterministic || len(res.Scheduled) != 1 {
		t.Fatalf("fleet of 1: %+v (%v) after %d model calls", res, err, r.model.calls())
	}
}

func TestRunner_DeepMaxCapsTheMonthAndTheRestGoesToTheNext(t *testing.T) {
	seeds := make([]int, 31)
	for i := range seeds {
		seeds[i] = i + 1
	}
	r := newRig(t, synth(40, seeds...), 30)
	r.runner.Triage = nil
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || len(res.Scheduled) != 30 || len(res.Deferred) != 1 {
		t.Fatalf("run: %d scheduled, deferred %v (%v)", len(res.Scheduled), res.Deferred, err)
	}
	waiting := res.Deferred[0]
	if row := r.ledger.get(res.PassID, waiting); row.state != StateTriaged ||
		row.deferred != "2026-12" {
		t.Fatalf("the 31st %s = %+v, want triaged for 2026-12", waiting, row)
	}
	adv, err := r.runner.Advance(context.Background())
	if err != nil || len(adv.Scheduled) != 0 {
		t.Fatalf("same month advance scheduled %d (%v)", len(adv.Scheduled), err)
	}
	r.clock.t = time.Date(2026, 12, 1, 0, 30, 0, 0, time.UTC)
	next, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || len(next.Scheduled) == 0 || next.Scheduled[0].Database != waiting ||
		next.Scheduled[0].Period != "2026-12" {
		t.Fatalf("next month: %+v (%v), want %s scheduled first", next.Scheduled, err, waiting)
	}
}

func TestRunner_MemberMovesThroughEveryState(t *testing.T) {
	r := newRig(t, synth(3, 2), 30)
	r.runner.Triage = nil
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || len(res.Scheduled) != 1 {
		t.Fatalf("run %+v (%v)", res, err)
	}
	if got := r.ledger.get(res.PassID, "db0001").state; got != StateRollup {
		t.Fatalf("a steady member is %s, want rollup", got)
	}
	if got := r.ledger.get(res.PassID, "db0002").state; got != StateDeepScheduled {
		t.Fatalf("the outlier is %s, want deep_scheduled", got)
	}
	adv, err := r.runner.Advance(context.Background())
	if err != nil || adv.Reviewed != 0 {
		t.Fatalf("advance before the review: %+v (%v)", adv, err)
	}
	r.deep.done[1] = 77
	adv, err = r.runner.Advance(context.Background())
	row := r.ledger.get(res.PassID, "db0002")
	if err != nil || adv.Reviewed != 1 || row.state != StateReviewed || row.runID != 77 {
		t.Fatalf("advance after the review: %+v row %+v (%v)", adv, row, err)
	}
}

func TestRunner_EmergencyStopHaltsSchedulingUntilRelease(t *testing.T) {
	r := newRig(t, synth(5, 1, 3), 30)
	r.runner.Triage = nil
	r.stopped.Store(true)
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || !res.Halted || len(r.deep.scheduled) != 0 ||
		r.ledger.passes[0].summary.Status != PassHalted {
		t.Fatalf("stopped run %+v (%v) scheduled %d", res, err, len(r.deep.scheduled))
	}
	if row := r.ledger.get(res.PassID, "db0003"); row.state != StateTriaged ||
		row.deferred != "2026-11" {
		t.Fatalf("a halted outlier is %+v, want triaged for this month", row)
	}
	adv, err := r.runner.Advance(context.Background())
	if err != nil || len(adv.Scheduled) != 0 || !adv.Halted {
		t.Fatalf("advance while stopped: %+v (%v)", adv, err)
	}
	r.stopped.Store(false)
	adv, err = r.runner.Advance(context.Background())
	if err != nil || len(adv.Scheduled) != 2 || len(r.deep.scheduled) != 2 {
		t.Fatalf("advance after release: %+v (%v)", adv, err)
	}
}

func TestRunner_SchedulerFailureKeepsTheMemberWaiting(t *testing.T) {
	r := newRig(t, synth(4, 1, 2), 30)
	r.runner.Triage = nil
	r.deep.fail = map[string]error{"db0001": errors.New("sage.review_trigger: permission denied")}
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || len(res.Scheduled) != 1 ||
		!strings.Contains(res.Failures["db0001"], "permission denied") {
		t.Fatalf("run %+v (%v), want db0001's failure kept", res, err)
	}
	if row := r.ledger.get(res.PassID, "db0001"); row.state != StateTriaged ||
		row.deferred != "2026-11" {
		t.Fatalf("failed member %+v, want triaged for this month", row)
	}
	r.deep.fail = nil
	adv, err := r.runner.Advance(context.Background())
	if err != nil || len(adv.Scheduled) != 1 || adv.Scheduled[0].Database != "db0001" {
		t.Fatalf("retry: %+v (%v)", adv, err)
	}
}

func TestRunner_LedgerErrorPropagates(t *testing.T) {
	r := newRig(t, synth(3, 1), 30)
	r.ledger.startErr = errors.New("connection refused")
	_, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err == nil || !strings.Contains(err.Error(), "start") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err %v, want the failed start with its cause", err)
	}
	if _, err := (&Runner{}).Run(context.Background(), []string{"a"}); err == nil {
		t.Fatal("a runner without a ledger ran")
	}
}

func TestRunner_DeepMaxZeroSchedulesNothing(t *testing.T) {
	r := newRig(t, synth(3, 1), 0)
	r.runner.Triage = nil
	res, err := r.runner.Run(context.Background(), names(r.src.rollups))
	if err != nil || len(res.Scheduled) != 0 || len(res.Deferred) != 1 {
		t.Fatalf("deep max 0: %+v (%v)", res, err)
	}
}

func TestCanMove_OnlyForward(t *testing.T) {
	ok := [][2]State{{StateRollup, StateTriaged}, {StateTriaged, StateDeepScheduled},
		{StateDeepScheduled, StateReviewed}}
	for _, p := range ok {
		if !CanMove(p[0], p[1]) {
			t.Errorf("%s -> %s refused", p[0], p[1])
		}
	}
	bad := [][2]State{{StateRollup, StateDeepScheduled}, {StateReviewed, StateTriaged},
		{StateTriaged, StateRollup}, {StateRollup, StateRollup}, {"", StateTriaged},
		{StateTriaged, "done"}}
	for _, p := range bad {
		if CanMove(p[0], p[1]) {
			t.Errorf("%s -> %s allowed", p[0], p[1])
		}
	}
}
