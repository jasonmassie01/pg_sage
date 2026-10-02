package slo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The engine's SLI sources on the real store: the recovery predicate
// over stored samples, database proxies and the Prometheus connector.

// The SLI recovery predicate over stored samples (CHECK-32).
func TestEngine_Recovery(t *testing.T) {
	f := newEngineFixture(t)
	cases := []struct {
		label string
		sql   string
		want  RecoveryState
		why   string
	}{
		{"healthy", `SELECT g, g * 0.0, g * 1000.0 FROM generate_series(1, 13) g`,
			RecoveryRecovered, ""},
		{"burning", `SELECT g, g * 50.0, g * 1000.0 FROM generate_series(1, 13) g`,
			RecoveryNotRecovered, ReasonBurning},
		{"reset", `SELECT g, 0.0, CASE WHEN g = 12 THEN 5.0 ELSE g * 1000.0 END
			FROM generate_series(1, 13) g`, RecoveryUnknown, ReasonCounterReset},
		{"low traffic", `SELECT g, 0.0, g * 10.0 FROM generate_series(1, 13) g`,
			RecoveryUnknown, ReasonLowTraffic},
	}
	for _, c := range cases {
		name := uniqueName("checkout")
		_, err := f.store.pool.Exec(f.ctx, `
			INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series, observed_at,
			    bad, eligible)
			SELECT $1, $2, 'app', now() - (13 - v.g) * interval '1 minute',
			       v.bad, v.eligible FROM (`+c.sql+`) AS v(g, bad, eligible)`,
			string(f.scope.DeploymentID), name)
		if err != nil {
			t.Fatalf("%s seed: %v", c.label, err)
		}
		e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
		r, err := e.Recovery(f.ctx, name, time.Now().Add(-12*time.Minute))
		if err != nil || r.State != c.want || r.Reason != c.why {
			t.Errorf("%s: recovery = %+v err=%v, want %s/%s", c.label, r, err, c.want, c.why)
		}
	}
	e := f.engine(t, []Objective{pushObjective(uniqueName("empty"))}, nil, nil)
	r, err := e.Recovery(f.ctx, e.objectives[0].Name, time.Now().Add(-10*time.Minute))
	if err != nil || r.State != RecoveryUnknown {
		t.Fatalf("no data: %+v err=%v", r, err)
	}
	if _, err := e.Recovery(f.ctx, "nope", time.Now()); !errors.Is(err, ErrUnknownSLO) {
		t.Fatalf("unknown SLO: err = %v", err)
	}
}

// fakeProxy returns scripted slices.
type fakeProxy struct {
	o     Objective
	slice ProxySlice
	calls int
}

func (p *fakeProxy) Objective() Objective { return p.o }
func (p *fakeProxy) Slice(context.Context, time.Time, History) ProxySlice {
	p.calls++
	return p.slice
}

func proxyObjective(name string) Objective {
	return Objective{Name: name, Kind: KindProxy, Source: SourceProxy, Proxy: name,
		Target: 0.99, Window: 30 * 24 * time.Hour, MinEligible: 3,
		StaleAfter: 5 * time.Minute}
}

// Proxy slices are stored as cumulative counters per database; a proxy
// that cannot measure (no replicas) makes the SLO unknown with its reason.
func TestEngine_ProxiesRecordCountersAndReasons(t *testing.T) {
	f := newEngineFixture(t)
	lagName := uniqueName("db_replication_lag")
	lag := &fakeProxy{o: proxyObjective(lagName), slice: ProxySlice{Reason: "no_replicas"}}
	conName := uniqueName("db_connection_refusal")
	con := &fakeProxy{o: proxyObjective(conName), slice: ProxySlice{Bad: 1, Eligible: 1}}
	e := f.engine(t, nil, []Proxy{lag, con}, nil)
	for i := 0; i < 3; i++ {
		if _, err := e.EvaluateOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if lag.calls != 3 || con.calls != 3 {
		t.Fatalf("proxy calls = %d, %d", lag.calls, con.calls)
	}
	last, ok, err := f.store.LastSample(f.ctx, f.scope.DeploymentID, conName,
		string(f.scope.DatabaseID))
	if err != nil || !ok || last.Bad != 3 || last.Eligible != 3 {
		t.Fatalf("cumulative proxy counter = %+v ok=%v err=%v", last, ok, err)
	}
	sts, _ := e.Statuses(f.ctx)
	for _, st := range sts {
		if st.Kind != KindProxy || st.CustomerImpact {
			t.Fatalf("proxy status = %+v", st)
		}
		if st.Name == lagName && !strings.Contains(strings.Join(st.Unknown, ","),
			"no_replicas") {
			t.Fatalf("lag proxy unknown = %v, want no_replicas", st.Unknown)
		}
	}
	// A new engine resumes the cumulative counter instead of restarting it.
	e2 := f.engine(t, nil, []Proxy{con}, nil)
	if _, err := e2.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	last, _, _ = f.store.LastSample(f.ctx, f.scope.DeploymentID, conName,
		string(f.scope.DatabaseID))
	if last.Eligible != 4 {
		t.Fatalf("resumed counter = %+v, want eligible 4", last)
	}
}

// A Prometheus SLO without a configured connector is unknown with the
// reason; with one, its windows come from the connector.
func TestEngine_PrometheusObjective(t *testing.T) {
	f := newEngineFixture(t)
	o := pushObjective(uniqueName("checkout"))
	o.Source = SourcePrometheus
	o.BadQuery, o.EligibleQuery = "bad[$window]", "eligible[$window]"
	none := f.engine(t, []Objective{o}, nil, nil)
	sts, err := none.EvaluateOnce(f.ctx)
	if err != nil || sts[0].State != StateUnknown ||
		strings.Join(sts[0].Unknown, ",") != ReasonConnectorNotConfigured {
		t.Fatalf("no connector: %+v err=%v", sts, err)
	}
	stub := &promStub{answer: func(_, q string) (int, string) {
		if strings.HasPrefix(q, "bad") {
			return 200, vector("200")
		}
		return 200, vector("10000")
	}}
	client := newClient(t, stub.server(t).URL, time.Second)
	e := f.engine(t, []Objective{o}, nil, func(d *EngineDeps) { d.Prometheus = client })
	sts, err = e.EvaluateOnce(f.ctx)
	if err != nil || sts[0].State != StatePage || !sts[0].CustomerImpact {
		t.Fatalf("prometheus burn 20x: %+v err=%v", sts, err)
	}
}


// historyProxy reads the baseline history the engine hands it.
type historyProxy struct {
	fakeProxy
	median float64
	n      int
	err    error
}

func (p *historyProxy) Slice(ctx context.Context, now time.Time, h History) ProxySlice {
	p.median, p.n, p.err = h.Baseline(ctx, now.Add(-time.Hour))
	v := 40.0
	return ProxySlice{Eligible: 1, Value: &v}
}

// A proxy's history is its own stored gauge values in this database.
func TestEngine_ProxyHistoryReadsOwnValues(t *testing.T) {
	f := newEngineFixture(t)
	p := &historyProxy{fakeProxy: fakeProxy{o: proxyObjective(uniqueName("db_latency"))}}
	e := f.engine(t, nil, []Proxy{p}, nil)
	for i := 0; i < 3; i++ {
		if _, err := e.EvaluateOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if p.err != nil || p.n != 2 || p.median != 40 {
		t.Fatalf("baseline seen on the third tick = %v over %d (err %v), want 40 over 2",
			p.median, p.n, p.err)
	}
}

func TestEngine_StatusAndTransitions(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("checkout")
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	st, err := e.Status(f.ctx, name)
	if err != nil || st.State != StateUnknown || len(st.Unknown) != 1 ||
		st.Unknown[0] != ReasonNotEvaluated {
		t.Fatalf("before evaluation: %+v err=%v", st, err)
	}
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	trs, err := e.Transitions(f.ctx, name, 10)
	if err != nil || len(trs) != 1 || trs[0].To != StateUnknown || trs[0].From != "" {
		t.Fatalf("transitions = %+v err=%v", trs, err)
	}
	for _, call := range []func() error{
		func() error { _, err := e.Status(f.ctx, "nope"); return err },
		func() error { _, err := e.Transitions(f.ctx, "nope", 1); return err },
	} {
		if err := call(); !errors.Is(err, ErrUnknownSLO) {
			t.Fatalf("unknown SLO: err = %v", err)
		}
	}
}

// Recovery of a Prometheus SLI comes from query_range slices; the
// pre-incident baseline from an instant query at the intervention time.
func TestEngine_RecoveryPrometheus(t *testing.T) {
	f := newEngineFixture(t)
	o := pushObjective(uniqueName("checkout"))
	o.Source = SourcePrometheus
	o.BadQuery, o.EligibleQuery = "bad[$window]", "eligible[$window]"
	end := time.Now().UTC().Truncate(time.Second)
	matrix := func(v string) string {
		var pts []string
		for i := 3; i >= 1; i-- {
			pts = append(pts, fmt.Sprintf(`[%d,%q]`,
				end.Add(-time.Duration(i)*RecoveryStep).Unix(), v))
		}
		return `{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{},"values":[` + strings.Join(pts, ",") + `]}]}}`
	}
	stub := &promStub{answer: func(path, q string) (int, string) {
		switch {
		case path == "/api/v1/query":
			return 200, vector("30000")
		case strings.HasPrefix(q, "bad"):
			return 200, matrix("0")
		}
		return 200, matrix("1000")
	}}
	client := newClient(t, stub.server(t).URL, time.Second)
	e := f.engine(t, []Objective{o}, nil, func(d *EngineDeps) { d.Prometheus = client })
	r, err := e.Recovery(f.ctx, o.Name, end.Add(-10*time.Minute))
	if err != nil || r.State != RecoveryRecovered || r.Evaluated != 3 {
		t.Fatalf("recovery = %+v err=%v", r, err)
	}
	stub.answer = func(path, q string) (int, string) {
		if path == "/api/v1/query" {
			return 200, vector("300000") // 10000 per slice before: traffic dropped
		}
		if strings.HasPrefix(q, "bad") {
			return 200, matrix("0")
		}
		return 200, matrix("1000")
	}
	r, _ = e.Recovery(f.ctx, o.Name, end.Add(-10*time.Minute))
	if r.State != RecoveryUnknown || r.Reason != ReasonTrafficDropped {
		t.Fatalf("traffic drop: %+v", r)
	}
}

// The error-budget source separates app SLIs from proxies: a proxy that
// cannot measure (no replicas) is unknown but is not an app SLO whose
// burn cannot be computed, so the autonomy layer can tell them apart.
func TestEngine_BudgetSummarySeparatesAppAndProxies(t *testing.T) {
	f := newEngineFixture(t)
	app := uniqueName("checkout")
	lag := &fakeProxy{o: proxyObjective(uniqueName("db_replication_lag")),
		slice: ProxySlice{Reason: ReasonNoReplicas}}
	e := f.engine(t, []Objective{pushObjective(app)}, []Proxy{lag}, nil)
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	sum, err := e.BudgetSummary(f.ctx)
	if err != nil || sum.AppSLOs != 1 || len(sum.Unknown) != 2 ||
		len(sum.UnknownApp) != 1 || sum.UnknownApp[0] != app || sum.FastBurning {
		t.Fatalf("summary = %+v err=%v", sum, err)
	}
	proxyOnly := f.engine(t, nil, []Proxy{lag}, nil)
	sum, _ = proxyOnly.BudgetSummary(f.ctx)
	if sum.AppSLOs != 0 || len(sum.UnknownApp) != 0 {
		t.Fatalf("proxy-only summary = %+v", sum)
	}
}
