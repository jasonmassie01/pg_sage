package slo

import (
	"context"
	stdslices "slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

func namespaceEngine(t *testing.T, f engineFixture, scope sre.Scope, name string, at time.Time, scoped bool) *Engine {
	t.Helper()
	o := pushObjective(name)
	o.Window = time.Hour
	e, err := NewEngine(EngineDeps{Database: "same-display-name", DatabaseScopedPush: scoped,
		Objectives: []Objective{o}, Store: f.store, Interval: time.Minute,
		Rules: []Rule{{SeverityPage, 5 * time.Minute, time.Minute, 14.4}},
		Scope: func(context.Context) (sre.Scope, error) { return scope, nil }, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func seedNamespacePush(t *testing.T, e *Engine, ctx context.Context, scope sre.Scope, name string, at time.Time, bad float64) {
	t.Helper()
	for i := 0; i <= 12; i++ {
		p := sample("same-actor-instance", float64(i)*bad, float64(i)*1000, at.Add(time.Duration(i-12)*30*time.Second))
		if created, err := e.PushScoped(ctx, scope, name, p); !created || err != nil {
			t.Fatalf("push %d: created=%v err=%v", i, created, err)
		}
	}
}

func TestDatabaseScopedPushSeparatesReusedDatabaseNamesAndLegacySamples(t *testing.T) {
	f := newEngineFixture(t)
	otherScope := bindScope(t, f.ctx, f.store.pool)
	name, at := uniqueName("reused"), time.Now().UTC().Truncate(time.Microsecond)
	name += strings.Repeat("x", 64-len(name))
	legacy := namespaceEngine(t, f, f.scope, name, at, false)
	a := namespaceEngine(t, f, f.scope, name, at, true)
	b := namespaceEngine(t, f, otherScope, name, at, true)
	if !a.DatabaseScopedPush() || legacy.DatabaseScopedPush() || len(a.sampleName(f.scope, a.objectives[0])) > 64 || a.sampleName(f.scope, a.objectives[0]) == b.sampleName(otherScope, b.objectives[0]) {
		t.Fatal("database namespaces collide")
	}
	seedNamespacePush(t, legacy, f.ctx, f.scope, name, at, 20)
	// Existing standalone persistence remains available through the default.
	if p, ok, err := legacy.ReadLastPushSample(f.ctx, name, "same-actor-instance"); err != nil || !ok || p.Bad != 240 {
		t.Fatalf("legacy data changed: %+v %v %v", p, ok, err)
	}
	if p, ok, err := a.ReadLastPushSample(f.ctx, name, "same-actor-instance"); err != nil || ok {
		t.Fatalf("legacy samples entered scoped engine: %+v %v %v", p, ok, err)
	}
	seedNamespacePush(t, a, f.ctx, f.scope, name, at, 20)
	for _, e := range []*Engine{a, b} {
		sts, err := e.EvaluateOnce(f.ctx)
		want := StatePage
		if e == b {
			want = StateUnknown
		}
		if err != nil || len(sts) != 1 || sts[0].State != want {
			t.Fatalf("scoped status: %+v %v want=%s", sts, err, want)
		}
	}
	if p, ok, err := b.ReadLastPushSample(f.ctx, name, "same-actor-instance"); err != nil || ok {
		t.Fatalf("old database identity entered replacement: %+v %v %v", p, ok, err)
	}
	if r, err := b.Recovery(f.ctx, name, at.Add(-6*time.Minute)); err != nil || r.State != RecoveryUnknown {
		t.Fatalf("old data proved recovery: %+v %v", r, err)
	}
	seedNamespacePush(t, b, f.ctx, otherScope, name, at, 0)
	for _, tc := range []struct {
		e     *Engine
		state RecoveryState
		why   string
	}{{a, RecoveryNotRecovered, ReasonBurning}, {b, RecoveryRecovered, ""}} {
		r, err := tc.e.Recovery(f.ctx, name, at.Add(-6*time.Minute))
		if err != nil || r.State != tc.state || r.Reason != tc.why {
			t.Fatalf("recovery mixed namespaces: %+v %v", r, err)
		}
	}
	if _, err := b.EvaluateOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{name, a.sampleName(f.scope, a.objectives[0]), b.sampleName(otherScope, b.objectives[0])} {
		if _, err := f.store.RecordSample(f.ctx, f.scope.DeploymentID, key, sample("old", 0, 1, at.Add(-26*time.Hour)), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []sre.Scope{f.scope, otherScope} {
		if _, err := f.store.pool.Exec(f.ctx, `INSERT INTO sage.sre_slo_transitions(deployment_id,database_id,name,to_state,at) VALUES($1,$2,$3,'unknown',$4)`, string(scope.DeploymentID), string(scope.DatabaseID), name, at.Add(-91*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	a.lastPurge = time.Time{}
	a.purge(f.ctx, f.scope, at)
	for _, tc := range []struct {
		key  string
		want bool
	}{{name, true}, {a.sampleName(f.scope, a.objectives[0]), false}, {b.sampleName(otherScope, b.objectives[0]), true}} {
		if _, ok, err := f.store.LastSample(f.ctx, f.scope.DeploymentID, tc.key, "old"); err != nil || ok != tc.want {
			t.Fatalf("retention crossed namespace: key=%s found=%v err=%v", tc.key, ok, err)
		}
	}
	var count int
	if err := f.store.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_slo_transitions WHERE deployment_id=$1 AND database_id=$2 AND name=$3 AND at<$4`, string(f.scope.DeploymentID), string(f.scope.DatabaseID), name, at.Add(-90*24*time.Hour)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("scoped transition retention failed: %d %v", count, err)
	}
	if err := f.store.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_slo_transitions WHERE deployment_id=$1 AND database_id=$2 AND name=$3 AND at<$4`, string(otherScope.DeploymentID), string(otherScope.DatabaseID), name, at.Add(-90*24*time.Hour)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other database transition deleted: %d %v", count, err)
	}
}

func TestDatabaseScopedPushDoesNotPublishCachedLegacyStatus(t *testing.T) {
	f := newEngineFixture(t)
	name, at := uniqueName("upgrade"), time.Now().UTC().Truncate(time.Microsecond)
	legacy := namespaceEngine(t, f, f.scope, name, at.Add(-time.Second), false)
	seedNamespacePush(t, legacy, f.ctx, f.scope, name, at.Add(-time.Second), 20)
	old, err := legacy.EvaluateOnce(f.ctx)
	if err != nil || len(old) != 1 || old[0].State != StatePage {
		t.Fatalf("legacy status seed: %+v %v", old, err)
	}
	scoped := namespaceEngine(t, f, f.scope, name, at, true)
	st, err := scoped.Status(f.ctx, name)
	if err != nil || st.State != StateUnknown || !stdslices.Contains(st.Unknown, ReasonNotEvaluated) || st.CustomerImpact || st.BudgetRemaining != nil || len(st.Rules) != 0 {
		t.Fatalf("cached legacy status published: %+v %v", st, err)
	}
	seedNamespacePush(t, scoped, f.ctx, f.scope, name, at, 20)
	fresh, err := scoped.EvaluateOnce(f.ctx)
	if err != nil || len(fresh) != 1 || fresh[0].State != StatePage || fresh[0].BurnStartedAt == nil || !fresh[0].BurnStartedAt.Equal(at) || fresh[0].DefinitionHash == old[0].DefinitionHash {
		t.Fatalf("scope upgrade retained legacy burn identity: %+v %v", fresh, err)
	}
	restarted := namespaceEngine(t, f, f.scope, name, at, true)
	st, err = restarted.Status(f.ctx, name)
	if err != nil || st.State != StatePage || st.BurnStartedAt == nil || !st.BurnStartedAt.Equal(at) {
		t.Fatalf("scoped persisted status lost: %+v %v", st, err)
	}
}
