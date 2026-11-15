package verify

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lab-01c: the index watch computes v2 beside v1. In shadow it acts on v1
// and stores v2 for comparison; in v2 it acts on v2 and stores v1. The
// look's alpha spend and the other verifier's record are part of the
// watch's Update, so a failed write leaves the watch as stored.

type fakeCounters struct {
	pre, post Series
	step      time.Time
	err       error
	reads     atomic.Int64
}

func pick(s Series, ids []int64) Series {
	out := Series{}
	for _, id := range ids {
		if b, ok := s[id]; ok {
			out[id] = b
		}
	}
	return out
}

func (f *fakeCounters) Window(_ context.Context, ids []int64, from, to time.Time,
	_ time.Duration) (CounterWindow, error) {
	f.reads.Add(1)
	if f.err != nil {
		return CounterWindow{}, f.err
	}
	if !to.After(f.step) {
		return CounterWindow{Series: pick(f.pre, ids)}, nil
	}
	return CounterWindow{Series: pick(f.post, ids)}, nil
}

func (f *fakeCounters) Seasonal(context.Context, []int64, time.Time, time.Time, int) (Series,
	error) {
	return Series{}, f.err
}

type fakeMechanisms struct{ calls atomic.Int64 }

func (f *fakeMechanisms) CheckMechanism(_ context.Context, in MechanismInput, _ time.Time) (
	Mechanism, error) {
	f.calls.Add(1)
	return Mechanism{Counted: in.Targets}, nil
}

type v2Fixture struct {
	source   *fakeObservationSource
	store    *memoryStateStore
	counters *fakeCounters
	mech     *fakeMechanisms
	request  WatchRequest
}

// newV2Fixture is a watch over the traffic-shift fixture: v1 sees a gain
// (its Pool reweights by the post-period mix), v2 sees no change.
func newV2Fixture() v2Fixture {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	source := newFakeObservationSource()
	source.executedAt = fxStep
	source.before = map[int64]Measurement{1: Summarize(toIntervals(in.Pre[1])),
		2: Summarize(toIntervals(in.Pre[2]))}
	source.after = map[int64]Measurement{1: Summarize(toIntervals(in.Post[1])),
		2: Summarize(toIntervals(in.Post[2]))}
	request := successfulWatchRequest("v2-watch")
	request.ExecutedAt = fxStep
	request.Criterion.TargetIDs = []int64{1, 2}
	return v2Fixture{source: source, store: newMemoryStateStore(),
		counters: &fakeCounters{pre: in.Pre, post: in.Post, step: fxStep},
		mech:     &fakeMechanisms{}, request: request}
}

func (f v2Fixture) engine(t *testing.T, method string, store StateStore) *Engine {
	t.Helper()
	options := DefaultOptions()
	options.Now = func() time.Time { return fxStep.Add(2 * time.Hour) }
	e, err := NewEngine(f.source, store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	err = e.EnableV2(V2Options{Counters: f.counters, Mechanisms: f.mech, Cycle: fxWidth,
		Method: func(context.Context) string { return method },
		Controls: func(context.Context, WatchState) ([]int64, error) {
			return []int64{11, 12, 13}, nil
		}})
	if err != nil {
		t.Fatalf("EnableV2: %v", err)
	}
	return e
}

func TestV2_EngineShadowActsOnV1AndStoresV2(t *testing.T) {
	f := newV2Fixture()
	v, err := f.engine(t, MethodShadow, f.store).Watch(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if v.Verifier != VerifierV1 || v.Outcome != OutcomeImproved || !v.Retain {
		t.Fatalf("shadow acted on %+v, want v1's improved verdict", v)
	}
	if v.Other == nil || v.Other.Verifier != VerifierV2 || v.Other.Outcome == OutcomeImproved {
		t.Fatalf("shadow's v2 record %+v, want a v2 verdict that is not improved", v.Other)
	}
	if _, ok := v.Evidence["v2"].(map[string]any); !ok {
		t.Fatalf("acted evidence lacks the v2 record: %v", v.Evidence)
	}
	stored, _ := f.store.Get(context.Background(), "v2-watch")
	if stored.Other == nil || stored.Other.Outcome != v.Other.Outcome ||
		stored.AlphaSpend.Looks != 1 {
		t.Fatalf("stored %+v / alpha %+v, want the v2 record and one look", stored.Other,
			stored.AlphaSpend)
	}
	w := stored.FrozenWeights
	if len(w) != 2 || w[1] != 0.5 || w[2] != 0.5 {
		t.Fatalf("frozen weights %v, want the pre-period shares 0.5/0.5", w)
	}
}

// v2 acts: the traffic shift is not credited; v1's improved verdict is
// kept for comparison.
func TestV2_EngineV2ActsOnV2(t *testing.T) {
	f := newV2Fixture()
	v, err := f.engine(t, MethodV2, f.store).Watch(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if v.Verifier != VerifierV2 || v.Outcome == OutcomeImproved {
		t.Fatalf("v2 acted on %+v, want a v2 verdict that is not improved", v)
	}
	if v.Other == nil || v.Other.Verifier != VerifierV1 || v.Other.Outcome != OutcomeImproved {
		t.Fatalf("v1 record %+v, want v1's improved verdict", v.Other)
	}
}

func TestV2_EngineV1ReadsNoCounters(t *testing.T) {
	f := newV2Fixture()
	v, err := f.engine(t, MethodV1, f.store).Watch(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if v.Verifier != VerifierV1 || v.Other != nil || f.counters.reads.Load() != 0 ||
		f.mech.calls.Load() != 0 {
		t.Fatalf("v1: %+v (other %+v), %d counter reads, %d mechanism checks; want none",
			v, v.Other, f.counters.reads.Load(), f.mech.calls.Load())
	}
}

// A v2 read failure in shadow is recorded as the v2 verdict and v1 still
// acts; in v2 it is an observation retry, never a revert.
func TestV2_EngineReadFailure(t *testing.T) {
	f := newV2Fixture()
	f.counters.err = errors.New("snapshot read timed out")
	v, err := f.engine(t, MethodShadow, f.store).Watch(context.Background(), f.request)
	if err != nil || v.Verifier != VerifierV1 || !v.Retain {
		t.Fatalf("shadow with a failed v2 read: %+v %v, want v1 acting", v, err)
	}
	if v.Other == nil || v.Other.Outcome != OutcomeInsufficient ||
		!strings.Contains(v.Other.Reason, "snapshot read timed out") {
		t.Fatalf("v2 record %+v, want insufficient naming the cause", v.Other)
	}
	g := newV2Fixture()
	g.counters.err = errors.New("snapshot read timed out")
	v, err = g.engine(t, MethodV2, g.store).Watch(context.Background(), g.request)
	if err != nil || v.Revert || v.Retain || v.Status != "extended" ||
		!strings.Contains(v.Reason, "observation_retry") {
		t.Fatalf("v2 with a failed read: %+v %v, want an observation retry", v, err)
	}
}

type failingUpdateStore struct{ *memoryStateStore }

func (s failingUpdateStore) Update(context.Context, WatchState) error {
	return errors.New("other_verdict write failed")
}

// Error propagation: a failed write of the v2 record fails the Update and
// leaves the watch as stored: no verdict is acted on without its record.
func TestV2_UpdateFailureLeavesTheWatchAsStored(t *testing.T) {
	f := newV2Fixture()
	store := failingUpdateStore{f.store}
	v, err := f.engine(t, MethodShadow, store).Watch(context.Background(), f.request)
	if err == nil || !strings.Contains(err.Error(), "other_verdict write failed") {
		t.Fatalf("err %v, want the update failure", err)
	}
	if v.Retain || v.Revert || v.Reason != "state_update_failed" {
		t.Fatalf("verdict %+v, want state_update_failed (neither retain nor revert)", v)
	}
	stored, _ := f.store.Get(context.Background(), "v2-watch")
	if stored.Other != nil || stored.AlphaSpend.Looks != 0 || stored.Status != "pending" {
		t.Fatalf("stored watch changed: %+v", stored)
	}
}

// Concurrency: two workers resuming the same watch under Claim store one
// v2 record (one look), not two.
func TestV2_ConcurrentResumeStoresOneRecord(t *testing.T) {
	f := newV2Fixture()
	state := WatchStateFromRequest(f.request)
	if err := f.store.Create(context.Background(), state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var wg sync.WaitGroup
	var claimed atomic.Int64
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, err := f.engine(t, MethodShadow, f.store).ResumeDueResults(
				context.Background())
			if err != nil {
				t.Errorf("ResumeDueResults: %v", err)
			}
			claimed.Add(int64(len(results)))
		}()
	}
	wg.Wait()
	stored, _ := f.store.Get(context.Background(), "v2-watch")
	if claimed.Load() != 1 || f.store.updated != 1 || stored.AlphaSpend.Looks != 1 ||
		stored.Other == nil {
		t.Fatalf("claimed %d, updates %d, looks %d, record %+v; want one of each",
			claimed.Load(), f.store.updated, stored.AlphaSpend.Looks, stored.Other)
	}
}

func TestV2_EnableV2RefusesMissingDependencies(t *testing.T) {
	e := newTestEngine(t, newFakeObservationSource(), newMemoryStateStore())
	if err := e.EnableV2(V2Options{}); err == nil {
		t.Fatal("EnableV2 accepted no counters, mechanisms or method")
	}
}

// A look that spends no alpha tests at an infinite interval: TCritical
// reads alpha 0 as the default 0.05, which would test an early look at
// the full alpha (review: the guard had no test).
func TestV2_ZeroAlphaLookNeverTestsAtTheDefault(t *testing.T) {
	for _, alpha := range []float64{0, -1, math.NaN()} {
		got := testAlpha(alpha)
		if !(got > 0) || got > 1e-300 {
			t.Fatalf("testAlpha(%v) = %v, want the smallest positive alpha", alpha, got)
		}
		if tc := TCritical(30, got); !math.IsInf(tc, 1) {
			t.Fatalf("TCritical(30, testAlpha(%v)) = %v, want +Inf", alpha, tc)
		}
	}
	if math.IsInf(TCritical(30, 0), 1) {
		t.Fatal("TCritical(30, 0) is infinite: the guard is no longer needed, revisit it")
	}
	if got := testAlpha(0.01); got != 0.01 {
		t.Fatalf("testAlpha(0.01) = %v, want it unchanged", got)
	}
}
