package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// playbooks-07 CHECK-08: GET /api/v1/sre/slos says a Prometheus SLO
// without a coverage query has unverified coverage; a pushed SLO's
// coverage is measured from its stored samples.
func TestSLOListReportsCoverageUnverified(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := signalInstance(t, mgr, "orders")
	if _, err := orders.slo.EvaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prom := promSLOInstance(t, mgr, "billing")
	if _, err := prom.EvaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := signalRouter(t, mgr, signalConfig(), testViewerUser())
	code, out := getJSON(t, h, "/api/v1/sre/slos")
	items, _ := out["slos"].([]any)
	if code != 200 || len(items) != 2 {
		t.Fatalf("slo list = %d %v", code, out)
	}
	seen := map[string]any{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		seen[fmt.Sprint(m["source"])] = m["coverage_unverified"]
	}
	if seen["prometheus"] != true || seen["push"] != false {
		t.Fatalf("coverage_unverified by source = %v", seen)
	}
}

// promSLOInstance registers a database whose only SLO is a Prometheus
// app SLI without a coverage query (and without a connector).
func promSLOInstance(t *testing.T, mgr *fleet.DatabaseManager, name string) *slo.Engine {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: lockChainRunner{},
		Config: sre.DefaultCoordinatorConfig(fmt.Sprintf("api-slo-cov:%s:%d", name,
			time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := slo.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	obj := slo.Objective{Name: "checkout-" + name, Kind: slo.KindApp,
		Source: slo.SourcePrometheus, Target: 0.999, Window: 30 * 24 * time.Hour,
		MinEligible: 50, BadQuery: "errors[$window]", EligibleQuery: "requests[$window]"}
	eng, err := slo.NewEngine(slo.EngineDeps{Database: name, Objectives: []slo.Objective{obj},
		Store: ss, Scope: func(context.Context) (sre.Scope, error) { return scope, nil },
		Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st),
		SLO: eng})
	return eng
}
