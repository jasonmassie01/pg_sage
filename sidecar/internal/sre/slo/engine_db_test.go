package slo

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The SLO engine on the real store: evaluates every SLO of one database,
// persists its error-budget state, opens an investigation on a page-level
// burn (once per burn), and answers the error-budget source (M7), the
// slo_status probe and the SLI recovery predicate.

type pageRecorder struct {
	mu    sync.Mutex
	pages []Status
	err   error
}

func (p *pageRecorder) handle(_ context.Context, st Status) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = append(p.pages, st)
	return p.err
}

func (p *pageRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pages)
}

type engineFixture struct {
	store *Store
	scope sre.Scope
	ctx   context.Context
	pages *pageRecorder
}

func newEngineFixture(t *testing.T) engineFixture {
	t.Helper()
	st, scope, ctx := liveStore(t)
	return engineFixture{store: st, scope: scope, ctx: ctx, pages: &pageRecorder{}}
}

func (f engineFixture) engine(t *testing.T, objs []Objective, proxies []Proxy,
	mutate func(d *EngineDeps)) *Engine {
	t.Helper()
	d := EngineDeps{Database: "orders", Objectives: objs, Proxies: proxies,
		Store: f.store, Interval: time.Minute, OnPage: f.pages.handle,
		Scope: func(context.Context) (sre.Scope, error) { return f.scope, nil }}
	if mutate != nil {
		mutate(&d)
	}
	e, err := NewEngine(d)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// seed writes 4 days of 5-minute cumulative samples: 1000 eligible events
// per step, bad at burn 0.2x (target 0.999), and burnFor at 20x for the
// last burnSteps steps.
func (f engineFixture) seed(t *testing.T, name string, burnSteps int) {
	t.Helper()
	_, err := f.store.pool.Exec(f.ctx, `
		INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series, observed_at,
		    bad, eligible)
		SELECT $1, $2, 'app', now() - interval '1 minute' - (1152 - g) * interval '5 minutes',
		       (CASE WHEN g > 1152 - $3 THEN (g - (1152 - $3)) * 20.0 ELSE 0 END)
		           + LEAST(g, 1152 - $3) * 0.2,
		       g * 1000.0
		FROM generate_series(1, 1152) g`,
		string(f.scope.DeploymentID), name, burnSteps)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func sample(series string, bad, eligible float64, at time.Time) PushSample {
	return PushSample{Series: series, Bad: bad, Eligible: eligible, ObservedAt: at}
}

func pushObjective(name string) Objective {
	o := appObjective()
	o.Name = name
	return o
}

func TestNewEngine_Validation(t *testing.T) {
	f := newEngineFixture(t)
	scope := func(context.Context) (sre.Scope, error) { return f.scope, nil }
	dup := pushObjective("dup")
	bad := pushObjective("bad")
	bad.Target = 2
	base := func(mutate func(d *EngineDeps)) EngineDeps {
		d := EngineDeps{Store: f.store, Scope: scope, Interval: time.Minute}
		mutate(&d)
		return d
	}
	cases := map[string]EngineDeps{
		"no store":    base(func(d *EngineDeps) { d.Store = nil }),
		"no scope":    base(func(d *EngineDeps) { d.Scope = nil }),
		"no interval": base(func(d *EngineDeps) { d.Interval = 0 }),
		"duplicate":   base(func(d *EngineDeps) { d.Objectives = []Objective{dup, dup} }),
		"invalid slo": base(func(d *EngineDeps) { d.Objectives = []Objective{bad} }),
		"invalid rule": base(func(d *EngineDeps) {
			d.Rules = []Rule{{SeverityPage, time.Minute, time.Hour, 1}}
		}),
	}
	for name, d := range cases {
		if _, err := NewEngine(d); err == nil {
			t.Errorf("%s: NewEngine succeeded", name)
		}
	}
	e, err := NewEngine(EngineDeps{Store: f.store, Scope: scope, Interval: time.Minute})
	if err != nil || len(e.Rules()) != 3 {
		t.Fatalf("defaults: %v rules=%v", err, e.Rules())
	}
}

func TestEngine_FastBurnPagesOnceAndPersists(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 12)
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	sts, err := e.EvaluateOnce(f.ctx)
	if err != nil || len(sts) != 1 {
		t.Fatalf("EvaluateOnce = %+v err=%v", sts, err)
	}
	st := sts[0]
	if st.State != StatePage || !st.FastBurning || !st.CustomerImpact ||
		st.BurnStartedAt == nil || st.Database != "orders" {
		t.Fatalf("status = %+v", st)
	}
	burn := st.Rules[0].Long.BurnRate
	if burn == nil || math.Abs(*burn-20) > 1.5 {
		t.Fatalf("1h burn = %v, want ~20", burn)
	}
	if st.Rules[1].Firing || st.Rules[2].Firing {
		t.Fatalf("slow rules fired: %+v", st.Rules)
	}
	if f.pages.count() != 1 {
		t.Fatalf("pages = %d, want 1", f.pages.count())
	}
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.pages.count() != 1 {
		t.Fatalf("a continuing burn paged again: %d", f.pages.count())
	}
	fresh := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	durable, err := fresh.Statuses(f.ctx)
	if err != nil || len(durable) != 1 || durable[0].State != StatePage ||
		durable[0].BurnStartedAt == nil || !durable[0].BurnStartedAt.Equal(*st.BurnStartedAt) {
		t.Fatalf("durable statuses = %+v err=%v", durable, err)
	}
}

// A failed page handler is retried on the next evaluation.
func TestEngine_PageHandlerRetried(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 12)
	f.pages.err = errors.New("coordinator unavailable")
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	for i := 0; i < 2; i++ {
		if _, err := e.EvaluateOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	f.pages.mu.Lock()
	f.pages.err = nil
	f.pages.mu.Unlock()
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.pages.count() != 3 {
		t.Fatalf("page calls = %d, want 2 failed + 1 accepted", f.pages.count())
	}
}

func TestEngine_HealthyIsOKAndBudgetKnown(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 0)
	o := pushObjective(name)
	o.Window = 72 * time.Hour
	e := f.engine(t, []Objective{o}, nil, nil)
	sts, err := e.EvaluateOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := sts[0]
	if st.State != StateOK || st.FastBurning || st.CustomerImpact || f.pages.count() != 0 {
		t.Fatalf("status = %+v", st)
	}
	if st.BudgetRemaining == nil || math.Abs(*st.BudgetRemaining-0.8) > 0.05 {
		t.Fatalf("budget remaining = %v, want ~0.8", st.BudgetRemaining)
	}
	sum, err := e.BudgetSummary(f.ctx)
	if err != nil || sum.FastBurning || sum.AppFastBurning || len(sum.Unknown) != 0 ||
		len(sum.SLOs) != 1 || sum.Database != "orders" {
		t.Fatalf("summary = %+v err=%v", sum, err)
	}
}

// No samples at all: every window is unknown (no data), never ok, and
// the error-budget source names the SLO as unknown.
func TestEngine_NoDataIsUnknown(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	sts, err := e.EvaluateOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sts[0].State != StateUnknown || strings.Join(sts[0].Unknown, ",") != ReasonNoData {
		t.Fatalf("status = %+v", sts[0])
	}
	sum, _ := e.BudgetSummary(f.ctx)
	if len(sum.Unknown) != 1 || sum.Unknown[0] != name || sum.FastBurning {
		t.Fatalf("summary = %+v", sum)
	}
}

// Read long after the last evaluation, a status is unknown
// (evaluation_stale): an old "ok" is not a current "ok".
func TestEngine_StaleEvaluationIsUnknown(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 0)
	first := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	if _, err := first.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	later := f.engine(t, []Objective{pushObjective(name)}, nil, func(d *EngineDeps) {
		d.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	})
	sts, err := later.Statuses(f.ctx)
	if err != nil || sts[0].State != StateUnknown ||
		!strings.Contains(strings.Join(sts[0].Unknown, ","), ReasonEvaluationStale) {
		t.Fatalf("stale read = %+v err=%v", sts, err)
	}
	sum, _ := later.BudgetSummary(f.ctx)
	if len(sum.Unknown) != 1 {
		t.Fatalf("summary of a stale evaluation = %+v", sum)
	}
}

func TestEngine_Push(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	prom := pushObjective(uniqueName("latency"))
	prom.Source = SourcePrometheus
	prom.BadQuery, prom.EligibleQuery = "b[$window]", "e[$window]"
	e := f.engine(t, []Objective{pushObjective(name), prom}, nil, nil)
	at := time.Now().UTC()
	p := PushSample{Series: "pod-a", Bad: 1, Eligible: 10, ObservedAt: at}
	if created, err := e.Push(f.ctx, name, p); err != nil || !created {
		t.Fatalf("push: %v created=%v", err, created)
	}
	if created, err := e.Push(f.ctx, name, p); err != nil || created {
		t.Fatalf("replayed push: %v created=%v", err, created)
	}
	if !e.HasPush(name) || e.HasPush(prom.Name) || e.HasPush("nope") {
		t.Fatal("HasPush is wrong")
	}
	cases := map[string]struct {
		name string
		p    PushSample
		want error
	}{
		"unknown":    {"nope", p, ErrUnknownSLO},
		"not push":   {prom.Name, p, ErrNotPush},
		"stale":      {name, sample("a", 0, 1, at.Add(-11*time.Minute)), ErrInvalidSample},
		"future":     {name, sample("a", 0, 1, at.Add(2*time.Minute)), ErrInvalidSample},
		"negative":   {name, sample("a", -1, 1, at), ErrInvalidSample},
		"nan":        {name, sample("a", 0, math.NaN(), at), ErrInvalidSample},
		"bad series": {name, sample("a b", 0, 1, at), ErrInvalidSample},
		"zero time":  {name, sample("a", 0, 1, time.Time{}), ErrInvalidSample},
	}
	for label, c := range cases {
		if _, err := e.Push(f.ctx, c.name, c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", label, err, c.want)
		}
	}
	empty := PushSample{Eligible: 5, ObservedAt: at.Add(time.Second)}
	if _, err := e.Push(f.ctx, name, empty); err != nil {
		t.Fatalf("default series: %v", err)
	}
	if _, ok, _ := f.store.LastSample(f.ctx, f.scope.DeploymentID, name, DefaultSeries); !ok {
		t.Fatal("an empty series was not stored as the default series")
	}
}

func TestEngine_Probe(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 12)
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	if res := e.Probe(f.ctx, probes.Args{}); res.Status != probes.StatusEmpty ||
		res.ProbeID != probes.SLOStatus {
		t.Fatalf("before any evaluation: %+v", res)
	}
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	res := e.Probe(f.ctx, probes.Args{})
	rows, err := probes.SLORows(res)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	r := rows[0]
	if r.Name != name || r.Kind != string(KindApp) || r.State != string(StatePage) ||
		!r.FastBurning || !r.CustomerImpact || r.LongWindow != "1h" ||
		r.ShortWindow != "5m" || math.Abs(r.BurnLong-20) > 1.5 {
		t.Fatalf("row = %+v", r)
	}
	broken := f.engine(t, nil, nil, func(d *EngineDeps) {
		d.Scope = func(context.Context) (sre.Scope, error) {
			return sre.Scope{}, sre.ErrMetadataUnavailable
		}
	})
	if res := broken.Probe(f.ctx, probes.Args{}); res.Status != probes.StatusError {
		t.Fatalf("unbound engine probe: %+v", res)
	}
}

// Evaluation, reads and pushes may run concurrently (-race).
func TestEngine_Concurrent(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	f.seed(t, name, 0)
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = e.EvaluateOnce(f.ctx) }()
		go func() { defer wg.Done(); _, _ = e.Statuses(f.ctx) }()
		go func(i int) {
			defer wg.Done()
			_, _ = e.Push(f.ctx, name, PushSample{Series: "c", Eligible: float64(i),
				ObservedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond)})
		}(i)
	}
	wg.Wait()
	sts, err := e.Statuses(f.ctx)
	if err != nil || len(sts) != 1 {
		t.Fatalf("after concurrent use: %+v err=%v", sts, err)
	}
}

// Run evaluates on its interval until the context ends.
func TestEngine_Run(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	e := f.engine(t, []Objective{pushObjective(name)}, nil, func(d *EngineDeps) {
		d.Interval = 20 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sts, _ := e.Statuses(f.ctx)
		if len(sts) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never evaluated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
