package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func TestSREDatabaseApplicationSignalsExcludeExternalSources(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, _, _ := f.registration(t)
	settings := sloSettings()
	settings.SLO.Enabled = true
	settings.ChangeEvents.FeedEnabled = true
	settings.SLO.Prometheus = config.SREPrometheusConfig{URL: "http://not-contacted.invalid", BearerTokenFile: "/missing-token", TimeoutSeconds: 3}
	settings.SLO.Proxies.Enabled = true
	settings.SLO.Objectives = append(settings.SLO.Objectives, config.SREObjectiveConfig{Name: "external", Database: "orders", Source: "prometheus", Target: 0.999, BadQuery: "bad[$window]", EligibleQuery: "all[$window]"})
	sig, err := newSRESignals(sreSignalsDeps{control: f.control, monitored: f.monitored, name: "orders", readPolicy: rt.sreReadPolicy(), settings: settings, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("database path touched external credentials: %v", err)
	}
	if sig.engine == nil || !sig.applicationOnly || sig.poller != nil {
		t.Fatalf("engine=%v appOnly=%v poller=%v", sig.engine, sig.applicationOnly, sig.poller)
	}
	if !sig.engine.HasPush("checkout") || sig.engine.HasPush("payments") {
		t.Fatal("unbound push included or explicit push missing")
	}
	sig.attach(rt.sre)
	statuses, err := sig.engine.EvaluateOnce(f.ctx)
	if err != nil || len(statuses) != 1 || statuses[0].Name != "checkout" {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}
	found := false
	for _, p := range sig.probes() {
		if p.ID == probes.SLOStatus {
			t.Fatal("external SLO source relabeled without policy")
		}
		if p.ID == probes.ApplicationSLOStatus {
			found = true
			r := p.Run(f.ctx, probes.Args{})
			if r.ProbeID != probes.ApplicationSLOStatus || r.Status != probes.StatusOK || len(r.Rows) != 1 || r.Rows[0]["source"] != "push" {
				t.Fatalf("application signal=%+v", r)
			}
		}
	}
	if !found {
		t.Fatal("missing database application signal")
	}
	// An explicit source allowlist still denies this new path.
	policy := rt.sreReadPolicy()
	policy.Allowed = []probes.ID{probes.StatTables}
	sig, err = newSRESignals(sreSignalsDeps{control: f.control, monitored: f.monitored, name: "orders", readPolicy: policy, settings: settings, logFn: func(string, string, ...any) {}})
	if err != nil || sig.engine != nil {
		t.Fatalf("narrowed engine=%v err=%v", sig.engine, err)
	}
}
