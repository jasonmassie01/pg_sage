package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-01d: the meta-db wiring of observe-only members.

type fakeObserveSets struct {
	res     policy.Resolution
	resErr  error
	docs    []policy.SetDocument
	setsErr error
}

func (f fakeObserveSets) Resolve(context.Context, int64) (policy.Resolution, error) {
	return f.res, f.resErr
}

func (f fakeObserveSets) Sets(context.Context) ([]policy.SetDocument, error) {
	return f.docs, f.setsErr
}

func setDoc(name, tier string) policy.SetDocument {
	return policy.SetDocument{Set: policy.PolicySet{Name: name,
		Requirements: policy.SetRequirements{Enrollment: policy.SetEnrollment{Tier: tier}}}}
}

// A promotion needs a set that says tier: full; every read failure, no
// set and a default tier keep the member observe-only (fail closed).
func TestObservePromotion_OnlyAnExplicitFullSetPromotes(t *testing.T) {
	rec := store.DatabaseRecord{ID: 9, Name: "obs"}
	docs := []policy.SetDocument{setDoc("prod", policy.EnrollTierFull),
		setDoc(policy.AgentCreatedSet, "")}
	cases := []struct {
		name string
		f    fakeObserveSets
		want bool
	}{
		{"explicit full", fakeObserveSets{res: policy.Resolution{Set: "prod"}, docs: docs}, true},
		{"agent-created default", fakeObserveSets{res: policy.Resolution{
			Set: policy.AgentCreatedSet}, docs: docs}, false},
		{"no set", fakeObserveSets{docs: docs}, false},
		{"set missing", fakeObserveSets{res: policy.Resolution{Set: "gone"}, docs: docs},
			false},
		{"resolve error", fakeObserveSets{res: policy.Resolution{Set: "prod"},
			resErr: errors.New("connection refused"), docs: docs}, false},
		{"sets error", fakeObserveSets{res: policy.Resolution{Set: "prod"},
			setsErr: errors.New("timeout")}, false},
	}
	for _, c := range cases {
		if got := observePromotion(context.Background(), c.f)(rec); got != c.want {
			t.Errorf("%s: promote %v, want %v", c.name, got, c.want)
		}
	}
}

// An observe-only row never gets a runtime: the meta reconcile leaves it
// out, and removes the runtime of a member demoted to observe-only.
func TestMetaReconcile_ObserveOnlyMemberGetsNoRuntime(t *testing.T) {
	testdb.SkipUnlessLive(t)
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "observe")
	setTier(t, env, rec.ID, store.TierObserveOnly)
	if report := env.reconcile(t); len(report.Added) != 0 ||
		fleetMgr.GetInstance("observe") != nil {
		t.Fatalf("observe-only pass = %+v, want no runtime", report)
	}
	setTier(t, env, rec.ID, store.TierFull)
	if report := env.reconcile(t); len(report.Added) != 1 {
		t.Fatalf("promoted pass = %+v, want the runtime added", report)
	}
	setTier(t, env, rec.ID, store.TierObserveOnly)
	if report := env.reconcile(t); len(report.Removed) != 1 ||
		fleetMgr.GetInstance("observe") != nil {
		t.Fatalf("demoted pass = %+v, want the runtime removed", report)
	}
}

func setTier(t *testing.T, env *metaReconcileEnv, id int, tier string) {
	t.Helper()
	if err := env.state.Store.SetTier(context.Background(), id, tier); err != nil {
		t.Fatalf("set tier %s: %v", tier, err)
	}
}
