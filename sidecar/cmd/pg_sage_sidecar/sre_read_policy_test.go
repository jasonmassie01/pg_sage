package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

func readPolicyRuntime(t *testing.T) *databaseRuntime {
	t.Helper()
	// Creating a lazy pool does not connect. The denied startup paths must
	// return without dialing or starting workers on this unreachable target.
	p, err := pgxpool.New(context.Background(), "postgres://policy@127.0.0.1:1/owned?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return &databaseRuntime{provider: "lakebase", cfg: config.DefaultConfig(),
		spec: databaseRuntimeSpec{Name: "owned", Scope: "test", Pool: p, ControlPool: p}}
}

func TestSREReadPolicyRuntimeDefaultsAndNarrowing(t *testing.T) {
	rt := readPolicyRuntime(t)
	p := rt.sreReadPolicy()
	if p.Validate() != nil || p.Scope != "database" || p.Database != "owned" || !p.Allows(probes.StatDatabase) || p.Allows(probes.ConnectionSaturation) {
		t.Fatalf("Lakebase default policy: %+v", p)
	}
	rt.provider = "self_hosted"
	if rt.sreReadPolicy() != nil {
		t.Fatal("standalone auto scope changed legacy behavior")
	}
	rt.cfg.SRE.ReadPolicy.AllowedSources = []string{}
	if rt.sreReadPolicy().Allows(probes.StatDatabase) {
		t.Fatal("explicit empty trusted allowlist widened access")
	}
	rt.cfg.SRE.ReadPolicy = config.SREReadPolicyConfig{Scope: "database", AllowedSources: []string{string(probes.StatDatabase)}}
	if !rt.sreReadPolicy().Allows(probes.StatDatabase) || rt.sreReadPolicy().Allows(probes.StatTables) {
		t.Fatal("trusted source allowlist did not narrow runtime")
	}
}

func TestSREReadPolicySuppressesIndependentRuntimeReads(t *testing.T) {
	rt := readPolicyRuntime(t)
	// If either path reaches its worker/dialing code these incomplete
	// dependencies fail; strict scope must stop it before construction.
	rt.executor = executor.New(nil, rt.cfg, time.Time{}, func(string, string, ...any) {})
	rt.sreService = sre.NewService("owned", nil, nil)
	rt.startRunways()
	rt.startSREActions()
	if rt.sreActions != nil || len(rt.features) != 0 {
		t.Fatalf("denied startup enabled components: %v", rt.features)
	}
	// Enabled external credentials/proxies must be discarded before either
	// credential loading or objective construction in database-only scope.
	rt.cfg.SRE.SLO.Prometheus = config.SREPrometheusConfig{URL: "http://not-contacted.invalid", BearerTokenFile: "/missing-token", TimeoutSeconds: 3}
	sig, err := newSRESignals(sreSignalsDeps{control: rt.spec.ControlPool, monitored: rt.spec.Pool,
		name: "owned", settings: rt.cfg.SRE, readPolicy: rt.sreReadPolicy(), logFn: func(string, string, ...any) {}})
	if err != nil || sig == nil || sig.feed == nil || sig.poller != nil || sig.engine == nil || !sig.applicationOnly || !sig.engine.DatabaseScopedPush() {
		t.Fatalf("strict signals: sig=%+v err=%v", sig, err)
	}
	if objectives := sig.engine.Objectives(); len(objectives) != 0 {
		t.Fatalf("database scope admitted external/proxy objectives: %+v", objectives)
	}
	var workers int
	sig.start(func(func()) { workers++ }, context.Background())
	if workers != 1 {
		t.Fatalf("expected only the application push engine worker, got %d", workers)
	}
	// An explicit allowlist that excludes application signals must still
	// suppress the engine and all independent signal workers entirely.
	policy := rt.sreReadPolicy()
	policy.Allowed = []probes.ID{probes.StatTables}
	narrow, err := newSRESignals(sreSignalsDeps{control: rt.spec.ControlPool, monitored: rt.spec.Pool,
		name: "owned", settings: rt.cfg.SRE, readPolicy: policy, logFn: func(string, string, ...any) {}})
	if err != nil || narrow.engine != nil || narrow.poller != nil {
		t.Fatalf("narrowed signals admitted a denied source: sig=%+v err=%v", narrow, err)
	}
	workers = 0
	narrow.start(func(func()) { workers++ }, context.Background())
	if workers != 0 {
		t.Fatalf("narrowed signals started %d denied workers", workers)
	}
	// A nonnil fanout with no source would panic on subscription; policy
	// must refuse the external source before touching it.
	rt.logFanout = logwatch.NewLogFanout(nil)
	if rt.logErrorCounter() != nil {
		t.Fatal("strict policy subscribed external log-derived SLO counter")
	}
}

func TestSREMissingLiveLogSourceRemainsUnknown(t *testing.T) {
	rt := readPolicyRuntime(t)
	rt.provider = "self_hosted"
	counter := rt.logErrorCounter()
	if counter != nil {
		t.Fatal("missing live fanout supplied a successful log source")
	}
	proxy := slo.NewErrorProxy(nil, counter, slo.DefaultProxyConfig())
	for i := 0; i < 2; i++ {
		slice := proxy.Slice(context.Background(), time.Now(), nil)
		if slice.Reason != slo.ReasonSourceUnavailable || slice.Eligible != 0 || slice.Bad != 0 {
			t.Fatalf("no source became a healthy zero-error window: %+v", slice)
		}
	}
}

func TestSREReadPolicyRefusesCustodianBeforeScan(t *testing.T) {
	for _, kind := range []sre.TriggerKind{sre.TriggerWraparound, sre.TriggerDiskWAL} {
		freeze, wal := &scanStub{}, &scanStub{}
		a := advisorWith(freeze, wal, &explainStub{})
		p := &probes.ReadPolicy{Version: 1, Scope: "database", Database: "owned", Binding: "binding"}
		_, err := a.Advise(probes.WithReadPolicy(context.Background(), p), sre.AdviceRequest{Kind: kind, Root: "inactive_slot"})
		if err == nil || !strings.Contains(err.Error(), probes.ReasonPolicyDenied) || freeze.scans != 0 || wal.scans != 0 {
			t.Fatalf("denied custodian read: freeze=%d wal=%d err=%v", freeze.scans, wal.scans, err)
		}
	}
	// A narrowed cluster allowlist authorizes only its own custodian.
	freeze, wal := &scanStub{}, &scanStub{}
	p := &probes.ReadPolicy{Version: 1, Scope: "cluster", Database: "owned", Binding: "binding", Allowed: []probes.ID{"custodian_freeze"}}
	_, err := advisorWith(freeze, wal, &explainStub{}).Advise(probes.WithReadPolicy(context.Background(), p), sre.AdviceRequest{Kind: sre.TriggerWraparound})
	if err != nil || freeze.scans != 1 || wal.scans != 0 {
		t.Fatalf("custodian source narrowing: freeze=%d wal=%d err=%v", freeze.scans, wal.scans, err)
	}
}
