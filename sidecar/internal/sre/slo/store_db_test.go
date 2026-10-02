package slo

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Durable SLO state (sage.sre_sli_samples, sage.sre_service_slos,
// sage.sre_slo_transitions): pushed and proxy counter samples are
// idempotent per series and time; windows aggregate them with reset
// compensation; the error-budget state per SLO and database survives
// restarts with its state history.

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func bindScope(t *testing.T, ctx context.Context, pool *pgxpool.Pool) sre.Scope {
	t.Helper()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.EnsureDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := st.BindDatabase(ctx, sre.Binding{DeploymentID: dep,
		RuntimeKey: "slo-test:" + string(sre.NewUUID()),
		Strength: sre.StrengthConfigured, ClusterEpoch: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func liveStore(t *testing.T) (*Store, sre.Scope, context.Context) {
	t.Helper()
	pool, ctx := livePool(t)
	st, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return st, bindScope(t, ctx, pool), ctx
}

func uniqueName(prefix string) string {
	return prefix + "-" + string(sre.NewUUID())[:8]
}

func TestNewStore_NilPool(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
}

func TestStore_RecordSampleIsIdempotent(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("checkout")
	at := time.Now().UTC().Truncate(time.Millisecond)
	s := PushSample{Series: "pod-a", Bad: 3, Eligible: 900, ObservedAt: at}
	created, err := st.RecordSample(ctx, scope.DeploymentID, name, s, nil)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	created, err = st.RecordSample(ctx, scope.DeploymentID, name, s, nil)
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v, want a duplicate", created, err)
	}
	last, ok, err := st.LastSample(ctx, scope.DeploymentID, name, "pod-a")
	if err != nil || !ok || last.Bad != 3 || last.Eligible != 900 ||
		!last.ObservedAt.Equal(at) {
		t.Fatalf("last = %+v ok=%v err=%v", last, ok, err)
	}
	if _, ok, err := st.LastSample(ctx, scope.DeploymentID, name, "pod-z"); ok || err != nil {
		t.Fatalf("unknown series: ok=%v err=%v", ok, err)
	}
	bad := s
	bad.ObservedAt = at.Add(time.Second)
	bad.Bad = 901
	if _, err := st.RecordSample(ctx, scope.DeploymentID, name, bad, nil); !errors.Is(err,
		ErrInvalidSample) {
		t.Fatalf("bad > eligible: err = %v", err)
	}
}

// Aggregate: increases between consecutive samples, the lookback sample
// as baseline, a decrease counted as a reset whose new value is the
// increase (Prometheus semantics), and series kept apart.
func TestStore_AggregateWithReset(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("checkout")
	end := time.Now().UTC().Truncate(time.Second)
	for _, s := range []PushSample{
		{"pod-a", 100, 10000, end.Add(-70 * time.Minute)},
		{"pod-a", 110, 11000, end.Add(-50 * time.Minute)},
		{"pod-a", 5, 500, end.Add(-30 * time.Minute)},
		{"pod-a", 25, 2500, end.Add(-10 * time.Minute)},
		{"pod-b", 0, 100, end.Add(-40 * time.Minute)},
		{"pod-b", 1, 300, end.Add(-5 * time.Minute)},
		{"pod-a", 999, 99999, end.Add(-3 * time.Hour)}, // before the lookback
	} {
		if _, err := st.RecordSample(ctx, scope.DeploymentID, name, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	aggs, err := st.Aggregate(ctx, scope.DeploymentID, name, "", end.Add(-time.Hour), end,
		15*time.Minute)
	if err != nil || len(aggs) != 2 {
		t.Fatalf("aggregate = %+v err=%v", aggs, err)
	}
	a, b := aggs[0], aggs[1]
	if a.Series != "pod-a" || a.Samples != 4 || a.Bad != 35 || a.Eligible != 3500 ||
		a.Resets != 1 || !a.First.Equal(end.Add(-70*time.Minute)) ||
		!a.Last.Equal(end.Add(-10*time.Minute)) {
		t.Fatalf("pod-a = %+v", a)
	}
	if b.Series != "pod-b" || b.Bad != 1 || b.Eligible != 200 || b.Resets != 0 {
		t.Fatalf("pod-b = %+v", b)
	}
	only, err := st.Aggregate(ctx, scope.DeploymentID, name, "pod-b", end.Add(-time.Hour),
		end, 0)
	if err != nil || len(only) != 1 || only[0].Series != "pod-b" {
		t.Fatalf("series filter = %+v err=%v", only, err)
	}
	none, err := st.Aggregate(ctx, scope.DeploymentID, uniqueName("nothing"),
		"", end.Add(-time.Hour), end, 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("no samples = %+v err=%v", none, err)
	}
}

func TestStore_Baseline(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("db_latency")
	end := time.Now().UTC().Truncate(time.Second)
	for i, v := range []float64{10, 20, 30, 40, 1000} {
		val := v
		s := PushSample{Series: "db", Bad: 0, Eligible: float64(i + 1),
			ObservedAt: end.Add(-time.Duration(10-i) * time.Minute)}
		if _, err := st.RecordSample(ctx, scope.DeploymentID, name, s, &val); err != nil {
			t.Fatal(err)
		}
	}
	median, n, err := st.Baseline(ctx, scope.DeploymentID, name, "db", end.Add(-time.Hour))
	if err != nil || n != 5 || median != 30 {
		t.Fatalf("baseline = %v over %d, err %v; want median 30 over 5", median, n, err)
	}
	_, n, err = st.Baseline(ctx, scope.DeploymentID, name, "db", end.Add(time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("empty baseline n=%d err=%v", n, err)
	}
}

func statusAt(name string, state State, at time.Time) Status {
	return Status{Name: name, Kind: KindApp, Source: SourcePush, Target: 0.999,
		Window: "30d", State: state, FastBurning: state == StatePage, EvaluatedAt: at,
		DefinitionHash: "h1", Unknown: []string{}}
}

// State history: since-times move only on a change; a burn (page or
// ticket) keeps its start time until the SLO stops burning.
func TestStore_SaveTracksTransitions(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("checkout")
	t1 := time.Now().UTC().Truncate(time.Millisecond)
	steps := []struct {
		state   State
		changed bool
		since   time.Time
		burn    *time.Time
	}{
		{StateOK, true, t1, nil},
		{StatePage, true, t1.Add(time.Minute), ptr(t1.Add(time.Minute))},
		{StatePage, false, t1.Add(time.Minute), ptr(t1.Add(time.Minute))},
		{StateTicket, true, t1.Add(3 * time.Minute), ptr(t1.Add(time.Minute))},
		{StateUnknown, true, t1.Add(4 * time.Minute), nil},
	}
	for i, s := range steps {
		saved, changed, err := st.Save(ctx, scope, statusAt(name, s.state,
			t1.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if changed != s.changed || !saved.StateSince.Equal(s.since) ||
			!sameTime(saved.BurnStartedAt, s.burn) {
			t.Fatalf("step %d (%s): changed=%v since=%s burn=%v", i, s.state, changed,
				saved.StateSince, saved.BurnStartedAt)
		}
	}
	got, err := st.Statuses(ctx, scope)
	if err != nil || len(got) != 1 || got[0].Name != name || got[0].State != StateUnknown {
		t.Fatalf("statuses = %+v err=%v", got, err)
	}
	trs, err := st.Transitions(ctx, scope, name, 10)
	if err != nil || len(trs) != 4 {
		t.Fatalf("transitions = %+v err=%v", trs, err)
	}
	if trs[0].To != StateUnknown || trs[0].From != StateTicket ||
		trs[3].From != "" || trs[3].To != StateOK {
		t.Fatalf("transitions newest first = %+v", trs)
	}
}

func ptr(t time.Time) *time.Time { return &t }

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// Statuses are per database: another database's SLO of the same name is
// its own row.
func TestStore_StatusesAreScoped(t *testing.T) {
	st, a, ctx := liveStore(t)
	b := bindScope(t, ctx, st.pool)
	name := uniqueName("checkout")
	if _, _, err := st.Save(ctx, a, statusAt(name, StatePage, time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Save(ctx, b, statusAt(name, StateOK, time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	ga, _ := st.Statuses(ctx, a)
	gb, _ := st.Statuses(ctx, b)
	if len(ga) != 1 || ga[0].State != StatePage || len(gb) != 1 || gb[0].State != StateOK {
		t.Fatalf("a = %+v b = %+v", ga, gb)
	}
	if _, _, err := st.Save(ctx, sre.Scope{}, statusAt(name, StateOK, time.Now())); err == nil {
		t.Fatal("saved under an invalid scope")
	}
}

// A saved status round-trips its rules, burn rates and budget.
func TestStore_StatusRoundTrip(t *testing.T) {
	st, scope, ctx := liveStore(t)
	o := appObjective()
	o.Name = uniqueName("checkout")
	in := Evaluate(o, DefaultRules(), windowsAt(allBurning(15)), known(30*24*time.Hour, 2),
		time.Now().UTC().Truncate(time.Millisecond))
	if _, _, err := st.Save(ctx, scope, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.Statuses(ctx, scope)
	if err != nil || len(got) != 1 {
		t.Fatalf("statuses = %+v err=%v", got, err)
	}
	g := got[0]
	if g.State != StatePage || !g.FastBurning || !g.CustomerImpact || len(g.Rules) != 3 ||
		g.Rules[0].Long.BurnRate == nil || math.Abs(*g.Rules[0].Long.BurnRate-15) > 1e-9 ||
		g.BudgetRemaining == nil || math.Abs(*g.BudgetRemaining+1) > 1e-9 {
		t.Fatalf("round trip = %+v", g)
	}
}

func TestStore_Purge(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("checkout")
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	fresh := time.Now().UTC().Add(-time.Minute)
	for _, at := range []time.Time{old, fresh} {
		if _, err := st.RecordSample(ctx, scope.DeploymentID, name,
			PushSample{Series: "s", Bad: 0, Eligible: 1, ObservedAt: at}, nil); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.Purge(ctx, scope.DeploymentID, time.Now().Add(-35*24*time.Hour),
		time.Now().Add(-90*24*time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("purge n=%d err=%v", n, err)
	}
	aggs, _ := st.Aggregate(ctx, scope.DeploymentID, name, "", old.Add(-time.Hour),
		time.Now(), 0)
	if len(aggs) != 1 || aggs[0].Samples != 1 || !aggs[0].First.Equal(fresh.Truncate(time.Microsecond)) {
		t.Fatalf("after purge = %+v", aggs)
	}
}
