package lakebaselive

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// This is a complete application-provided denominator, separate from sampled
// provider log/metric history. It creates only uniquely named owned SLO rows
// and a durable read-only SRE acceptance case in the guarded native database.
func TestLakebasePushedApplicationSLOLifecycle(t *testing.T) {
	p, ctx, policy := sreNativePool(t)
	st, err := sre.NewPostgresStore(p, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	name := "acceptance_push_" + string(sre.NewUUID())
	cfg := sre.DefaultCoordinatorConfig("native-push-slo:" + string(sre.NewUUID()))
	cfg.AutomaticStart = false
	var engine *slo.Engine
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Config: cfg, ReadPolicy: policy,
		Runner: probes.NewRunner(p, probes.Catalog(), nil, policy),
		Signals: []sre.SignalProbe{{ID: probes.ApplicationSLOStatus, Run: func(ctx context.Context, args probes.Args) probes.Result {
			r := engine.Probe(ctx, args)
			r.ProbeID, r.Version = probes.ApplicationSLOStatus, "v1_bound_push"
			return r
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sloStore, err := slo.NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := now
	obj := slo.Objective{Name: name, Kind: slo.KindApp, Source: slo.SourcePush, Target: .999,
		Window: time.Hour, MinEligible: 100, StaleAfter: 2 * time.Minute}
	var caseID sre.UUID
	pages := 0
	series := "actor.native-acceptance." + string(scope.DatabaseID)
	deps := slo.EngineDeps{Database: policy.Database, DatabaseScopedPush: true, Store: sloStore, Objectives: []slo.Objective{obj},
		Rules: []slo.Rule{{Severity: slo.SeverityPage, Long: 5 * time.Minute, Short: time.Minute, Factor: 14.4}},
		Scope: coord.Bind, Interval: time.Minute, Now: func() time.Time { return clock },
		OnPage: func(ctx context.Context, status slo.Status) error {
			inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "acceptance-push:" + name, Kind: sre.TriggerSLOBurn,
				Subject: name, IdempotencyKey: name + ":" + status.BurnStartedAt.UTC().Format(time.RFC3339Nano), Actor: "native-acceptance"})
			if err == nil {
				pages++
				caseID = inv.ID
			}
			return err
		}}
	engine, err = slo.NewEngine(deps)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is restricted to this unique acceptance SLO; keep the durable
	// case, evidence and event chain available for independent readback.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := p.Exec(cleanup, `DELETE FROM sage.sre_sli_samples WHERE deployment_id=$1 AND series=$2`, string(scope.DeploymentID), series); err != nil {
			t.Errorf("cleanup owned SLI samples: %v", err)
		}
		if _, err := p.Exec(cleanup, `DELETE FROM sage.sre_service_slos WHERE deployment_id=$1 AND database_id=$2 AND name=$3`, string(scope.DeploymentID), string(scope.DatabaseID), name); err != nil {
			t.Errorf("cleanup owned SLO status: %v", err)
		}
		var remaining int
		if err := p.QueryRow(cleanup, `SELECT (SELECT count(*) FROM sage.sre_sli_samples WHERE deployment_id=$1 AND series=$2) + (SELECT count(*) FROM sage.sre_service_slos WHERE deployment_id=$1 AND database_id=$3 AND name=$4) + (SELECT count(*) FROM sage.sre_slo_transitions WHERE deployment_id=$1 AND database_id=$3 AND name=$4)`, string(scope.DeploymentID), series, string(scope.DatabaseID), name).Scan(&remaining); err != nil || remaining != 0 {
			t.Errorf("owned SLO fixture cleanup: remaining=%d err=%v", remaining, err)
		}
	})
	var before int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.action_log`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var last slo.PushSample
	for i := 0; i <= 12; i++ {
		last = slo.PushSample{Series: series, ObservedAt: now.Add(time.Duration(i-12) * 30 * time.Second), Bad: float64(i * 20), Eligible: float64(i * 1000)}
		if created, err := engine.PushScoped(ctx, scope, name, last); !created || err != nil {
			t.Fatalf("sample %d created=%v err=%v", i, created, err)
		}
	}
	if created, err := engine.PushScoped(ctx, scope, name, last); created || err != nil {
		t.Fatalf("identical replay: created=%v err=%v", created, err)
	}
	conflict := last
	conflict.Bad++
	if _, err := engine.PushScoped(ctx, scope, name, conflict); !errors.Is(err, slo.ErrSampleConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
	foreign := scope
	foreign.DatabaseID = sre.NewUUID()
	if _, err := engine.PushScoped(ctx, foreign, name, last); !errors.Is(err, slo.ErrScopeMismatch) {
		t.Fatalf("foreign registered identity accepted: %v", err)
	}
	if stored, ok, err := engine.ReadLastPushSample(ctx, name, series); err != nil || !ok || stored.Bad != last.Bad || stored.Eligible != last.Eligible {
		t.Fatalf("scoped durable readback: %+v %v %v", stored, ok, err)
	}
	for i := 0; i < 2; i++ {
		status, err := engine.EvaluateOnce(ctx)
		if err != nil || len(status) != 1 || status[0].State != slo.StatePage || !status[0].CustomerImpact {
			t.Fatalf("burn evaluation: %+v %v", status, err)
		}
	}
	if pages != 1 || caseID == "" {
		t.Fatalf("burn did not create exactly one case: pages=%d case=%s", pages, caseID)
	}
	if err := coord.Investigate(ctx, caseID); err != nil {
		t.Fatal(err)
	}
	inv, err := st.Get(ctx, scope, caseID)
	if err != nil || !inv.State.Terminal() || inv.State == sre.StateFailed || inv.Summary.CustomerImpact == nil || inv.Summary.CustomerImpact.State != causal.ImpactBurning || inv.Summary.CustomerImpact.SLO != name {
		t.Fatalf("application burn did not become durable scoped SRE evidence: %+v %v", inv, err)
	}
	evidence, err := st.Evidence(ctx, scope, caseID)
	if err != nil {
		t.Fatal(err)
	}
	var cited bool
	for _, e := range evidence {
		if !e.VerifyHash() {
			t.Fatal("evidence hash invalid")
		}
		if e.ID == inv.Summary.CustomerImpact.EvidenceID {
			var res probes.Result
			if json.Unmarshal(e.Payload, &res) != nil || res.ProbeID != probes.ApplicationSLOStatus {
				t.Fatal("customer impact cites another source")
			}
			rows, err := probes.SLORows(res)
			cited = err == nil && len(rows) == 1 && rows[0].Source == "push" && rows[0].Name == name
		}
	}
	if !cited || st.VerifyEvents(ctx, scope, caseID) != nil {
		t.Fatal("durable citation/event chain invalid")
	}
	// A new engine reads persisted status; old evaluations and old samples
	// independently become unknown rather than remaining healthy/burning.
	deps.OnPage = nil
	restarted, err := slo.NewEngine(deps)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := restarted.Status(ctx, name)
	if err != nil || saved.State != slo.StatePage {
		t.Fatalf("restart lost persistent burn: %+v %v", saved, err)
	}
	clock = now.Add(4 * time.Minute)
	saved, err = restarted.Status(ctx, name)
	if err != nil || saved.State != slo.StateUnknown || !slices.Contains(saved.Unknown, slo.ReasonEvaluationStale) {
		t.Fatalf("old evaluation retained live claim: %+v %v", saved, err)
	}
	status, err := restarted.EvaluateOnce(ctx)
	if err != nil || len(status) != 1 || status[0].State != slo.StateUnknown || !slices.Contains(status[0].Unknown, slo.ReasonStale) {
		t.Fatalf("stale samples retained live claim: %+v %v", status, err)
	}
	var after, sampleCount int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.action_log`).Scan(&after); err != nil || after != before {
		t.Fatalf("read-only case modified action ledger: before=%d after=%d err=%v", before, after, err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.sre_sli_samples WHERE deployment_id=$1 AND series=$2`, string(scope.DeploymentID), series).Scan(&sampleCount); err != nil || sampleCount != 13 {
		t.Fatalf("replay or scope check changed samples: n=%d err=%v", sampleCount, err)
	}
	t.Logf("case=%s database=%s source=push samples=%d pages=%d replay=deduplicated conflict=refused wrong_scope=refused restart=persisted stale=unknown action_ledger_unchanged=true", caseID, policy.Database, sampleCount, pages)
}
