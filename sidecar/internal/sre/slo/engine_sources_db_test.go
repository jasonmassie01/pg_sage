package slo

import (
	"context"
	"errors"
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

