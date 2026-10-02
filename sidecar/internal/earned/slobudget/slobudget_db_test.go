package slobudget

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The earned-autonomy error-budget signal read from Sage SRE M5's real SLO
// engine (coordinator mapping, 2026-10-02): any page-level burn, database
// proxies included, downgrades; an unknown registered app SLO downgrades;
// an unknown proxy does not (proxies are often unknown for structural
// reasons: no standbys, no log access); no app SLO and no burn is no
// budget at all.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/earned/slobudget"))
}

type fixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	store *slo.Store
	scope sre.Scope
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.EnsureDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := st.BindDatabase(ctx, sre.Binding{DeploymentID: dep,
		RuntimeKey: "slobudget-test:" + string(sre.NewUUID()),
		Strength:   sre.StrengthConfigured, ClusterEpoch: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := slo.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{ctx: ctx, pool: pool, store: store, scope: scope}
}

func (f fixture) engine(t *testing.T, objs []slo.Objective, proxies []slo.Proxy) *slo.Engine {
	t.Helper()
	e, err := slo.NewEngine(slo.EngineDeps{Database: "orders", Objectives: objs,
		Proxies: proxies, Store: f.store, Interval: time.Minute,
		Scope: func(context.Context) (sre.Scope, error) { return f.scope, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluateOnce(f.ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return e
}

func unique(prefix string) string { return prefix + "-" + string(sre.NewUUID())[:8] }

func appObjective(name string) slo.Objective {
	return slo.Objective{Name: name, Kind: slo.KindApp, Source: slo.SourcePush,
		Target: 0.999, Window: 30 * 24 * time.Hour, MinEligible: 100,
		StaleAfter: 5 * time.Minute}
}

func proxyObjective(name string) slo.Objective {
	return slo.Objective{Name: name, Kind: slo.KindProxy, Source: slo.SourceProxy,
		Proxy: name, Target: 0.99, Window: 30 * 24 * time.Hour, MinEligible: 3,
		StaleAfter: 5 * time.Minute}
}

type stubProxy struct {
	o     slo.Objective
	slice slo.ProxySlice
}

func (p stubProxy) Objective() slo.Objective { return p.o }
func (p stubProxy) Slice(context.Context, time.Time, slo.History) slo.ProxySlice {
	return p.slice
}

// seedBurn writes 4 days of 5-minute cumulative samples for one series:
// 1000 eligible events per step, a bad fraction of badFrac in the last 12
// steps (an hour) and none before.
func (f fixture) seedBurn(t *testing.T, name, series string, badFrac float64) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series, observed_at,
		    bad, eligible)
		SELECT $1, $2, $3, now() - interval '1 minute' - (1152 - g) * interval '5 minutes',
		       GREATEST(g - 1140, 0) * 1000.0 * $4, g * 1000.0
		FROM generate_series(1, 1152) g`,
		string(f.scope.DeploymentID), name, series, badFrac); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func budget(t *testing.T, f fixture, e *slo.Engine, database string) earned.BudgetState {
	t.Helper()
	src := New(e)
	if src == nil {
		t.Fatal("a running engine gave no budget source")
	}
	st, err := src.ErrorBudget(f.ctx, database)
	if err != nil {
		t.Fatalf("error budget: %v", err)
	}
	return st
}

func TestProxyUnknownIsNoDowngrade(t *testing.T) {
	f := newFixture(t)
	lag := stubProxy{o: proxyObjective(unique("db_replication_lag")),
		slice: slo.ProxySlice{Reason: slo.ReasonNoReplicas}}
	e := f.engine(t, nil, []slo.Proxy{lag})
	st := budget(t, f, e, "orders")
	if st.Configured || st.FastBurning || st.Unknown {
		t.Fatalf("proxy-only unknown = %+v, want no budget", st)
	}
}

func TestAppUnknownDowngrades(t *testing.T) {
	f := newFixture(t)
	lag := stubProxy{o: proxyObjective(unique("db_replication_lag")),
		slice: slo.ProxySlice{Reason: slo.ReasonNoReplicas}}
	app := unique("checkout")
	e := f.engine(t, []slo.Objective{appObjective(app)}, []slo.Proxy{lag})
	st := budget(t, f, e, "orders")
	if !st.Configured || !st.Unknown || st.FastBurning {
		t.Fatalf("app unknown = %+v, want an unknown budget", st)
	}
}

func TestProxyFastBurnDowngrades(t *testing.T) {
	f := newFixture(t)
	name := unique("db_connection_refusal")
	f.seedBurn(t, name, string(f.scope.DatabaseID), 0.2) // 20x the 1% budget
	burning := stubProxy{o: proxyObjective(name),
		slice: slo.ProxySlice{Bad: 200, Eligible: 1000}}
	e := f.engine(t, nil, []slo.Proxy{burning})
	st := budget(t, f, e, "orders")
	if !st.Configured || !st.FastBurning {
		t.Fatalf("proxy fast burn = %+v, want a burn", st)
	}
}

func TestAppFastBurnDowngrades(t *testing.T) {
	f := newFixture(t)
	app := unique("checkout")
	f.seedBurn(t, app, "app", 0.02) // 20x the 0.1% budget
	e := f.engine(t, []slo.Objective{appObjective(app)}, nil)
	st := budget(t, f, e, "orders")
	if !st.Configured || !st.FastBurning || st.Unknown {
		t.Fatalf("app fast burn = %+v", st)
	}
}

func TestNoSLOsIsNoBudget(t *testing.T) {
	f := newFixture(t)
	e := f.engine(t, nil, nil)
	if st := budget(t, f, e, "orders"); st.Configured || st.FastBurning || st.Unknown {
		t.Fatalf("no SLOs = %+v", st)
	}
	if New(nil) != nil {
		t.Fatal("a nil engine gave a budget source (it must mean no SLO subsystem)")
	}
}

func TestAnotherDatabasesEngineIsRefused(t *testing.T) {
	f := newFixture(t)
	e := f.engine(t, nil, nil)
	if _, err := New(e).ErrorBudget(f.ctx, "billing"); err == nil {
		t.Fatal("the orders engine answered for billing")
	}
}
