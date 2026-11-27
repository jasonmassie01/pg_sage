package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01d: the observe-only sweeper against a fake meta store and a
// fake visit (no database). The real catalog reads are in
// observe_only_db_test.go.

type fakeObserveStore struct {
	mu        sync.Mutex
	members   []store.DatabaseRecord
	listErr   error
	dsnErr    map[int]error
	recordErr error
	tierErr   error
	rounds    int
	sweeps    map[int]store.ObserveSweep
	finished  []store.ObserveRound
	tiers     map[int]string
}

func newFakeObserveStore(n int) *fakeObserveStore {
	f := &fakeObserveStore{sweeps: map[int]store.ObserveSweep{}, tiers: map[int]string{},
		dsnErr: map[int]error{}}
	for i := 1; i <= n; i++ {
		f.members = append(f.members, store.DatabaseRecord{ID: i,
			Name: fmt.Sprintf("obs-%d", i), Enabled: true, Tier: store.TierObserveOnly})
	}
	return f
}

func (f *fakeObserveStore) ObserveOnlyMembers(context.Context) ([]store.DatabaseRecord,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.DatabaseRecord
	for _, m := range f.members {
		if f.tiers[m.ID] == "" || f.tiers[m.ID] == store.TierObserveOnly {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeObserveStore) GetConnectionString(_ context.Context, id int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.dsnErr[id]; err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://u:secret@h/db%d", id), nil
}

func (f *fakeObserveStore) BeginObserveRound(context.Context, time.Time, int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rounds++
	return int64(f.rounds), nil
}

func (f *fakeObserveStore) RecordObserveSweep(_ context.Context, sw store.ObserveSweep) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.sweeps[sw.DatabaseID] = sw
	return nil
}

func (f *fakeObserveStore) FinishObserveRound(_ context.Context, r store.ObserveRound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, r)
	return nil
}

func (f *fakeObserveStore) SetTier(_ context.Context, id int, tier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tierErr != nil {
		return f.tierErr
	}
	f.tiers[id] = tier
	return nil
}

func (f *fakeObserveStore) sweep(id int) (store.ObserveSweep, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sw, ok := f.sweeps[id]
	return sw, ok
}

// gradeVisit grades every member B after delay, counting visits and the
// peak number running at once.
type gradeVisit struct {
	delay  time.Duration
	hold   chan struct{} // when set, every visit waits for it to close
	fail   map[int]error
	calls  atomic.Int64
	active atomic.Int64
	peak   atomic.Int64
	dsns   sync.Map
}

func (g *gradeVisit) visit(ctx context.Context, rec store.DatabaseRecord,
	dsn string) (ObserveGrade, error) {
	g.calls.Add(1)
	n := g.active.Add(1)
	defer g.active.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.dsns.Store(rec.ID, dsn)
	if g.hold != nil {
		<-g.hold
	}
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
		return ObserveGrade{}, ctx.Err()
	}
	if err := g.fail[rec.ID]; err != nil {
		return ObserveGrade{}, err
	}
	return ObserveGrade{Letter: "B", Detail: json.RawMessage(`{"overall":"B"}`)}, nil
}

func newTestSweeper(st ObserveStore, g *gradeVisit, opts ObserveOptions) *ObserveSweeper {
	opts.Visit = g.visit
	return NewObserveSweeper(st, opts)
}

func TestObserveOnly_ZeroMembersNoSweeperWork(t *testing.T) {
	st := newFakeObserveStore(0)
	g := &gradeVisit{}
	s := newTestSweeper(st, g, ObserveOptions{})
	round, err := s.SweepRound(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.Members != 0 || round.ID != 0 || g.calls.Load() != 0 || st.rounds != 0 ||
		len(st.finished) != 0 {
		t.Fatalf("round %+v, %d visits, %d rounds written: want no work", round,
			g.calls.Load(), st.rounds)
	}
}

func TestObserveOnly_ConcurrencyFourIsNeverExceeded(t *testing.T) {
	st := newFakeObserveStore(40)
	g := &gradeVisit{delay: 15 * time.Millisecond}
	s := newTestSweeper(st, g, ObserveOptions{})
	round, err := s.SweepRound(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if peak := g.peak.Load(); peak != DefaultObserveConcurrency {
		t.Fatalf("peak %d members at once, want exactly %d", peak, DefaultObserveConcurrency)
	}
	if round.MaxConcurrent < 1 || round.MaxConcurrent > DefaultObserveConcurrency {
		t.Fatalf("round max_concurrent %d, want 1-%d", round.MaxConcurrent,
			DefaultObserveConcurrency)
	}
	if round.Members != 40 || round.OK != 40 || round.Unreachable != 0 ||
		round.Failed != 0 || g.calls.Load() != 40 {
		t.Fatalf("round %+v after %d visits, want 40 ok", round, g.calls.Load())
	}
	sw, ok := st.sweep(7)
	if !ok || sw.Status != store.ObserveOK || sw.Letter != "B" || sw.RoundID != round.ID ||
		string(sw.Grade) != `{"overall":"B"}` {
		t.Fatalf("member 7 sweep %+v (ok=%v)", sw, ok)
	}
	if dsn, _ := g.dsns.Load(7); dsn != "postgres://u:secret@h/db7" {
		t.Fatalf("member 7 was visited with %v", dsn)
	}
	if len(st.finished) != 1 || st.finished[0].FinishedAt.Before(st.finished[0].StartedAt) {
		t.Fatalf("finished rounds %+v", st.finished)
	}
}

func TestObserveOnly_OneUnreachableMemberAndTheRoundContinues(t *testing.T) {
	st := newFakeObserveStore(6)
	g := &gradeVisit{fail: map[int]error{
		3: fmt.Errorf("%w: dial tcp: connection refused", ErrObserveUnreachable),
		5: errors.New("first look: permission denied for pg_class"),
	}}
	st.dsnErr[6] = errors.New("decrypt: wrong key")
	s := newTestSweeper(st, g, ObserveOptions{})
	round, err := s.SweepRound(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.OK != 3 || round.Unreachable != 1 || round.Failed != 2 || round.Members != 6 {
		t.Fatalf("round %+v, want 3 ok, 1 unreachable, 2 failed", round)
	}
	if sw, _ := st.sweep(3); sw.Status != store.ObserveUnreachable ||
		!strings.Contains(sw.Error, "connection refused") || sw.Letter != "" {
		t.Fatalf("member 3 %+v, want unreachable with the dial error", sw)
	}
	if sw, _ := st.sweep(5); sw.Status != store.ObserveFailed ||
		!strings.Contains(sw.Error, "permission denied") {
		t.Fatalf("member 5 %+v, want failed with the check error", sw)
	}
	sw, _ := st.sweep(6)
	if sw.Status != store.ObserveFailed || !strings.Contains(sw.Error, "wrong key") ||
		strings.Contains(sw.Error, "secret") {
		t.Fatalf("member 6 %+v, want failed reading its credentials", sw)
	}
	if sw, _ := st.sweep(4); sw.Status != store.ObserveOK {
		t.Fatalf("member 4 after the failures: %+v, want ok", sw)
	}
}

func TestObserveOnly_ActionRequestIsRefused(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{Name: "full-a",
		Config: config.DatabaseConfig{Name: "full-a"}})
	st := newFakeObserveStore(2)
	s := newTestSweeper(st, &gradeVisit{}, ObserveOptions{Members: mgr.SetObserveOnly})
	if _, err := s.SweepRound(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
	_, err := mgr.ActionTarget("obs-1")
	if !errors.Is(err, ErrObserveOnlyMember) ||
		!strings.Contains(err.Error(), "observe-only member") {
		t.Fatalf("action on obs-1: %v, want the observe-only refusal", err)
	}
	if inst, err := mgr.ActionTarget("full-a"); err != nil || inst == nil ||
		inst.Name != "full-a" {
		t.Fatalf("action on a full member: %v %v", inst, err)
	}
	if _, err := mgr.ActionTarget("nope"); !errors.Is(err, ErrActionTargetNotFound) {
		t.Fatalf("action on an unknown database: %v", err)
	}
	if _, err := mgr.ActionTarget(""); !errors.Is(err, ErrActionTargetNotFound) {
		t.Fatalf("action on an empty name: %v", err)
	}
}

func TestObserveOnly_OptionDefaultsAndBounds(t *testing.T) {
	cases := []struct {
		in, want int
	}{{0, 4}, {-3, 4}, {1, 1}, {4, 4}, {16, 16}, {17, 16}, {1000, 16}}
	for _, c := range cases {
		s := NewObserveSweeper(newFakeObserveStore(0), ObserveOptions{Concurrency: c.in})
		if got := s.Concurrency(); got != c.want {
			t.Errorf("concurrency %d -> %d, want %d", c.in, got, c.want)
		}
	}
	s := NewObserveSweeper(newFakeObserveStore(0), ObserveOptions{})
	if s.Interval() != DefaultObserveInterval || DefaultObserveInterval != 15*time.Minute {
		t.Fatalf("interval %s, want 15m", s.Interval())
	}
	s = NewObserveSweeper(newFakeObserveStore(0), ObserveOptions{Interval: time.Second})
	if s.Interval() != MinObserveInterval {
		t.Fatalf("interval 1s -> %s, want the %s floor", s.Interval(), MinObserveInterval)
	}
	if NewObserveSweeper(nil, ObserveOptions{}) != nil {
		t.Fatal("a sweeper without a store")
	}
}

func TestObserveOnly_PolicySetPromotesAMember(t *testing.T) {
	st := newFakeObserveStore(3)
	g := &gradeVisit{}
	promote := func(rec store.DatabaseRecord) bool { return rec.ID == 2 }
	mgr := NewManager(&config.Config{Mode: "fleet"})
	s := newTestSweeper(st, g, ObserveOptions{Promote: promote, Members: mgr.SetObserveOnly})
	round, err := s.SweepRound(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.Promoted != 1 || round.Members != 2 || g.calls.Load() != 2 {
		t.Fatalf("round %+v after %d visits, want 1 promoted and 2 swept", round,
			g.calls.Load())
	}
	if st.tiers[2] != store.TierFull {
		t.Fatalf("member 2 tier %q, want full", st.tiers[2])
	}
	if _, swept := st.sweep(2); swept {
		t.Fatal("the promoted member was swept as observe-only")
	}
	if _, err := mgr.ActionTarget("obs-2"); errors.Is(err, ErrObserveOnlyMember) {
		t.Fatal("a promoted member is still refused as observe-only")
	}
}

func TestObserveOnly_PromotionAtTheCapKeepsTheMemberObserved(t *testing.T) {
	st := newFakeObserveStore(2)
	st.tierErr = fmt.Errorf("%w: maximum of 50 databases reached", store.ErrValidation)
	g := &gradeVisit{}
	var logged []string
	s := newTestSweeper(st, g, ObserveOptions{
		Promote: func(store.DatabaseRecord) bool { return true },
		Logf: func(_ string, f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}})
	round, err := s.SweepRound(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if round.Promoted != 0 || round.OK != 2 || g.calls.Load() != 2 {
		t.Fatalf("round %+v, want both still swept and none promoted", round)
	}
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "\n"),
		"maximum of 50 databases reached") {
		t.Fatalf("logged %q, want the cap named", logged)
	}
}

func TestObserveOnly_ListErrorPropagates(t *testing.T) {
	st := newFakeObserveStore(2)
	st.listErr = errors.New("connection refused")
	g := &gradeVisit{}
	_, err := newTestSweeper(st, g, ObserveOptions{}).SweepRound(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listing observe-only members") ||
		!strings.Contains(err.Error(), "connection refused") || g.calls.Load() != 0 {
		t.Fatalf("round: %v after %d visits", err, g.calls.Load())
	}
}

func TestObserveOnly_RecordErrorDoesNotStopTheRound(t *testing.T) {
	st := newFakeObserveStore(5)
	st.recordErr = errors.New("meta db: disk full")
	g := &gradeVisit{}
	round, err := newTestSweeper(st, g, ObserveOptions{}).SweepRound(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("round error %v, want the record error", err)
	}
	if g.calls.Load() != 5 || round.OK != 5 {
		t.Fatalf("%d visits, round %+v: want all 5 visited", g.calls.Load(), round)
	}
}

func TestObserveOnly_CanceledRoundStopsVisiting(t *testing.T) {
	st := newFakeObserveStore(50)
	g := &gradeVisit{delay: 50 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := newTestSweeper(st, g, ObserveOptions{}).SweepRound(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("round: %v, want the deadline", err)
	}
	if n := g.calls.Load(); n >= 50 {
		t.Fatalf("%d of 50 visited after the deadline", n)
	}
}

func TestObserveOnly_RoundsNeverOverlap(t *testing.T) {
	st := newFakeObserveStore(8)
	g := &gradeVisit{hold: make(chan struct{})}
	s := newTestSweeper(st, g, ObserveOptions{})
	first := make(chan error, 1)
	go func() {
		_, err := s.SweepRound(context.Background())
		first <- err
	}()
	for g.calls.Load() == 0 { // the first round is visiting
		time.Sleep(time.Millisecond)
	}
	if _, err := s.SweepRound(context.Background()); !errors.Is(err,
		ErrObserveRoundRunning) {
		t.Fatalf("second round while the first runs: %v", err)
	}
	close(g.hold)
	if err := <-first; err != nil {
		t.Fatalf("first round: %v", err)
	}
	if g.calls.Load() != 8 {
		t.Fatalf("%d visits, want 8", g.calls.Load())
	}
}

func TestObserveOnly_ObserveToFullToDisabledAcrossRounds(t *testing.T) {
	st := newFakeObserveStore(2)
	g := &gradeVisit{}
	mgr := NewManager(&config.Config{Mode: "fleet"})
	s := newTestSweeper(st, g, ObserveOptions{Members: mgr.SetObserveOnly})
	if _, err := s.SweepRound(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if _, err := mgr.ActionTarget("obs-1"); !errors.Is(err, ErrObserveOnlyMember) {
		t.Fatalf("round 1: obs-1 %v, want observe-only", err)
	}
	st.tiers[1] = store.TierFull // promoted elsewhere
	if r, err := s.SweepRound(context.Background()); err != nil || r.Members != 1 {
		t.Fatalf("round 2: %+v %v, want only obs-2", r, err)
	}
	if _, err := mgr.ActionTarget("obs-1"); errors.Is(err, ErrObserveOnlyMember) {
		t.Fatal("round 2: a full member is still refused as observe-only")
	}
	st.mu.Lock()
	st.members = st.members[:0] // obs-2 disabled, obs-1 full
	st.mu.Unlock()
	if r, err := s.SweepRound(context.Background()); err != nil || r.Members != 0 {
		t.Fatalf("round 3: %+v %v, want nothing swept", r, err)
	}
	if _, err := mgr.ActionTarget("obs-2"); errors.Is(err, ErrObserveOnlyMember) {
		t.Fatal("round 3: a disabled member is still listed as observe-only")
	}
}

func TestObserveOnly_RunStopsWithItsContext(t *testing.T) {
	st := newFakeObserveStore(1)
	g := &gradeVisit{}
	ticks := make(chan time.Time)
	s := newTestSweeper(st, g, ObserveOptions{
		After: func(time.Duration) <-chan time.Time { return ticks }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	ticks <- time.Now() // the first round ran; this starts the second
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if n := g.calls.Load(); n < 1 || n > 2 {
		t.Fatalf("%d visits, want 1 or 2 rounds of one member", n)
	}
}
