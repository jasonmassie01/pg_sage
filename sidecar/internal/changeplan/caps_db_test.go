package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// plan-02b: the engine checks the plan caps and the peak windows before
// each step, over a real database with the engine fixture's fakes.

// fakeSizes is the database size and each index's live size.
type fakeSizes struct {
	mu     sync.Mutex
	db     int64
	byName map[string]int64
	err    error
	reads  int
}

func (z *fakeSizes) DatabaseBytes(context.Context) (int64, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.reads++
	return z.db, z.err
}

func (z *fakeSizes) IndexBytes(_ context.Context, names []string) (int64, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.reads++
	if z.err != nil {
		return 0, z.err
	}
	var n int64
	for _, name := range names {
		n += z.byName[name]
	}
	return n, nil
}

// fixtureSizes reads f.sizes when a test set it, else the live sizes.
type fixtureSizes struct{ f *engineFixture }

func (s fixtureSizes) DatabaseBytes(ctx context.Context) (int64, error) {
	if s.f.sizes != nil {
		return s.f.sizes.DatabaseBytes(ctx)
	}
	return NewDBSizes(s.f.pool).DatabaseBytes(ctx)
}

func (s fixtureSizes) IndexBytes(ctx context.Context, names []string) (int64, error) {
	if s.f.sizes != nil {
		return s.f.sizes.IndexBytes(ctx, names)
	}
	return NewDBSizes(s.f.pool).IndexBytes(ctx, names)
}

func (f *engineFixture) readCaps(context.Context) (policy.PlanCaps, error) {
	return f.caps, f.capsErr
}

// defaultCaps is a fixture under the policy defaults.
func defaultCaps(t *testing.T, label string) *engineFixture {
	f := newEngineFixture(t, label)
	f.caps = policy.PlanCaps{}
	return f
}

func started(s Step) bool {
	return s.Runtime.State != StatePending && s.Runtime.State != StateAwaitingApproval
}

func TestCaps_TwoIndexesAndASettingRunUnderTheDefaults(t *testing.T) {
	f := defaultCaps(t, "caps_happy")
	in := threeStepPlan()
	// It cites a finding: a finding-less step cannot be queued (plan-02a).
	in.Steps[2].FindingID = ptr(int64(13))
	p := f.approved(in)
	got := f.runUntil(p.ID, "improved", 60)
	if got.Status != StatusCompleted {
		t.Fatalf("plan %s under the default caps, want completed: %+v", got.Status, got.Steps)
	}
}

// The proposal is refused with the cap and the numbers; so is an approval
// once the caps tightened.
func TestCaps_ProposalOverTheIndexCapIsRefused(t *testing.T) {
	f := defaultCaps(t, "caps_propose")
	_, err := f.store.Create(f.ctx, chain(3, 101), newFakeResolver())
	if !errors.Is(err, ErrOverCap) || !strings.Contains(err.Error(),
		"max_new_indexes_per_table") || !strings.Contains(err.Error(), "public.orders") {
		t.Fatalf("create 3 indexes on one table: %v, want the per-table cap", err)
	}
	f.caps = policy.PlanCaps{MaxNewIndexesPerTable: 3}
	p := mustCreate(t, f.store, chain(3, 101))
	f.caps = policy.PlanCaps{}
	if _, err := f.store.Approve(f.ctx, p.ID, p.PlanHash, 7); !errors.Is(err, ErrOverCap) {
		t.Fatalf("approve under tighter caps: %v, want the per-table cap", err)
	}
	if got := f.get(p.ID); got.Status != StatusProposed {
		t.Fatalf("refused approval left the plan %s, want proposed", got.Status)
	}
	f.capsErr = errors.New("policy store down")
	if _, err := f.store.Approve(f.ctx, p.ID, p.PlanHash, 7); err == nil ||
		!strings.Contains(err.Error(), "policy store down") {
		t.Fatalf("approve without caps: %v, want refused with the read error", err)
	}
}

// Error propagation: a size read that fails makes the step wait with
// size_unavailable, never pass the cap; a caps read that fails waits too.
func TestCaps_SizeReadFailureWaits(t *testing.T) {
	f := defaultCaps(t, "caps_size_fail")
	f.sizes = &fakeSizes{db: 1 << 30, err: errors.New("pg_database_size timed out")}
	p := f.approved(chain(1, 101))
	for range 3 {
		f.tick()
	}
	s := f.get(p.ID).Steps[0].Runtime
	if s.State != StatePending || !strings.HasPrefix(s.StateReason, ReasonSizeUnavailable) ||
		!strings.Contains(s.StateReason, "timed out") || f.manual.callCount() != 0 {
		t.Fatalf("step %+v, runs %d: want pending with size_unavailable", s,
			f.manual.callCount())
	}
	f.sizes.err, f.capsErr = nil, errors.New("policy store down")
	f.tick()
	s = f.get(p.ID).Steps[0].Runtime
	if s.State != StatePending || !strings.Contains(s.StateReason, "policy store down") {
		t.Fatalf("step %+v: want pending with the caps read error", s)
	}
	f.capsErr = nil
	f.tick()
	if s := f.get(p.ID).Steps[0]; !started(s) {
		t.Fatalf("step %+v after the reads recover, want started", s.Runtime)
	}
}

// Boundary: index bytes at exactly 10% of the database let the next index
// start; one byte over makes it wait.
func TestCaps_IndexBytesBoundaryBeforeAStep(t *testing.T) {
	for _, c := range []struct {
		built int64
		start bool
	}{{1000, true}, {1001, false}} {
		f := defaultCaps(t, "caps_bytes")
		f.sizes = &fakeSizes{db: 10000, byName: map[string]int64{"public.idx_101_1": c.built}}
		p := f.approved(chain(2, 101))
		got := f.runUntil(p.ID, "improved", 12)
		s2 := got.Steps[1]
		if started(s2) != c.start {
			t.Fatalf("built %d of 10000: step 2 %+v, want started=%v", c.built, s2.Runtime,
				c.start)
		}
		if !c.start && !strings.HasPrefix(s2.Runtime.StateReason, CapIndexBytesPct) {
			t.Fatalf("waiting step 2 reason %q, want the byte cap", s2.Runtime.StateReason)
		}
	}
}

// Boundary: a step at the first minute of a peak window waits; at the
// minute the window closes it runs.
func TestCaps_PeakWindowBoundary(t *testing.T) {
	f := defaultCaps(t, "caps_peak")
	f.caps.PeakWindows = []string{"12:00-13:00 UTC"} // the fixture clock starts at 12:00
	p := f.approved(chain(1, 101))
	f.tick()
	s := f.get(p.ID).Steps[0].Runtime
	if s.State != StatePending || !strings.HasPrefix(s.StateReason, ReasonPeakWindow) {
		t.Fatalf("at 12:00 step %+v, want pending in the peak window", s)
	}
	f.clock.Add(59 * time.Minute)
	f.tick()
	if s := f.get(p.ID).Steps[0]; started(s) {
		t.Fatalf("at 12:59 step %+v, want still waiting", s.Runtime)
	}
	f.clock.Add(time.Minute)
	f.tick()
	if s := f.get(p.ID).Steps[0]; !started(s) || f.manual.callCount() != 1 {
		t.Fatalf("at 13:00 step %+v (runs %d), want started", s.Runtime, f.manual.callCount())
	}
}

// A step that waits past its deadline ends expired and on_expired runs:
// halt_ask (the default) halts the plan; keep lets it complete.
func TestCaps_WaitPastTheDeadlineExpires(t *testing.T) {
	for _, onExpired := range []OutcomeAction{ActionHaltAsk, ActionKeep} {
		f := defaultCaps(t, "caps_expire")
		f.caps.PeakWindows = []string{"always"}
		in := chain(1, 101)
		in.Steps[0].OnExpired = onExpired
		p := f.approved(in) // deadline_minutes 60
		f.tick()
		f.clock.Add(60 * time.Minute)
		f.tick()
		if s := f.get(p.ID).Steps[0].Runtime; s.State != StatePending {
			t.Fatalf("%s: at the deadline step %+v, want still waiting", onExpired, s)
		}
		f.clock.Add(time.Second)
		f.tick()
		f.tick()
		got := f.get(p.ID)
		s := got.Steps[0].Runtime
		if s.State != StateSkipped || s.Verdict != OutcomeExpired || s.VerdictAt == nil ||
			f.manual.callCount() != 0 {
			t.Fatalf("%s: past the deadline step %+v, want skipped as expired", onExpired, s)
		}
		want := StatusHalted
		if onExpired == ActionKeep {
			want = StatusCompleted
		}
		if got.Status != want {
			t.Fatalf("%s: plan %s, want %s", onExpired, got.Status, want)
		}
	}
}

// State transition: a step waiting on the byte cap starts once a revert
// frees the bytes. Plan a's index is built and being judged (counted);
// plan b's step waits until a's step is reverted.
func TestCaps_RevertFreesTheBytes(t *testing.T) {
	f := defaultCaps(t, "caps_revert")
	f.sizes = &fakeSizes{db: 10000, byName: map[string]int64{"public.idx_101_1": 2000}}
	a := f.approved(chain(1, 101))
	f.tick()
	f.tick()
	if s := f.get(a.ID).Steps[0].Runtime; s.State != StateJudging {
		t.Fatalf("plan a step %+v, want judging", s)
	}
	b := f.approved(chain(1, 102))
	f.tick()
	if s := f.get(b.ID).Steps[0].Runtime; s.State != StatePending ||
		!strings.HasPrefix(s.StateReason, CapIndexBytesPct) {
		t.Fatalf("plan b step %+v, want waiting on the byte cap", s)
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.plan_step SET state = 'reverted'
		WHERE plan_id = %d`, a.ID))
	f.tick()
	if s := f.get(b.ID).Steps[0]; !started(s) {
		t.Fatalf("plan b step %+v after the revert, want started", s.Runtime)
	}
}

// Concurrency: two plans on one table, each under the cap, together over
// it: the third index to start waits (per-database caps count every
// plan's steps), and no tick ever has more than 2 started on the table.
func TestCaps_TwoPlansTogetherOverTheCap(t *testing.T) {
	f := defaultCaps(t, "caps_two_plans")
	a := f.approved(chain(2, 101))
	b := f.approved(chain(2, 102))
	for range 30 {
		f.judgeAll(a.ID, "improved", "monitoring")
		f.judgeAll(b.ID, "improved", "monitoring")
		f.tick()
		f.clock.Add(time.Minute)
		n := 0
		for _, id := range []int64{a.ID, b.ID} {
			for _, s := range f.get(id).Steps {
				if started(s) {
					n++
				}
			}
		}
		if n > 2 {
			t.Fatalf("%d index steps started on public.orders, cap 2", n)
		}
	}
	waiting := 0
	for _, id := range []int64{a.ID, b.ID} {
		s := f.get(id).Steps[1].Runtime
		if s.State == StatePending && strings.HasPrefix(s.StateReason, CapIndexesPerTable) {
			waiting++
		}
	}
	if waiting != 2 || f.manual.callCount() != 2 {
		t.Fatalf("%d second steps waiting on the per-table cap, %d runs; want 2 and 2",
			waiting, f.manual.callCount())
	}
}

// Integration: live index sizes on a fixture drive the byte cap. The
// database's own size bounds the cap, so a 1% cap with a large index
// waits and a 50% cap lets the step start.
func TestCaps_LiveIndexSizesDriveTheByteCap(t *testing.T) {
	f := defaultCaps(t, "caps_live")
	mustExecPlan(t, f, `CREATE TABLE public.orders AS SELECT g AS id, g % 97 AS c1,
		md5(g::text) AS c2 FROM generate_series(1, 200000) g`)
	mustExecPlan(t, f, `CREATE INDEX idx_101_1 ON public.orders (c2)`)
	z := NewDBSizes(f.pool)
	db, err := z.DatabaseBytes(f.ctx)
	if err != nil || db <= 0 {
		t.Fatalf("database size %d: %v", db, err)
	}
	built, err := z.IndexBytes(f.ctx, []string{"public.idx_101_1", "public.missing_idx"})
	if err != nil || built < 1<<20 {
		t.Fatalf("live index bytes %d: %v, want the built index's size", built, err)
	}
	f.caps.MaxNewIndexBytesPct = 1
	p := f.approved(chain(2, 101))
	got := f.runUntil(p.ID, "improved", 12)
	if s := got.Steps[1].Runtime; s.State != StatePending ||
		!strings.HasPrefix(s.StateReason, CapIndexBytesPct) {
		t.Fatalf("1%% cap, %d of %d bytes: step 2 %+v, want waiting", built, db, s)
	}
	f.caps.MaxNewIndexBytesPct = 50
	f.tick()
	if s := f.get(p.ID).Steps[1]; !started(s) {
		t.Fatalf("50%% cap, %d of %d bytes: step 2 %+v, want started", built, db, s.Runtime)
	}
}
