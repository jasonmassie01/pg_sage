package slo

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

func TestStoreSampleReplayCollisionIsAtomic(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("replay")
	p := sample("actor:1", 3, 100, time.Now().UTC().Truncate(time.Microsecond))
	v := 12.0
	created, err := st.RecordSample(ctx, scope.DeploymentID, name, p, &v)
	if err != nil || !created {
		t.Fatalf("initial: %v %v", created, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := p
			if i%2 == 1 {
				q.Bad++
			}
			created, err := st.RecordSample(ctx, scope.DeploymentID, name, q, &v)
			if created || i%2 == 0 && err != nil || i%2 == 1 && !errors.Is(err, ErrSampleConflict) {
				t.Errorf("retry %d: created=%v err=%v", i, created, err)
			}
		}(i)
	}
	wg.Wait()
	for _, value := range []*float64{nil, valuePtr(13)} {
		if _, err := st.RecordSample(ctx, scope.DeploymentID, name, p, value); !errors.Is(err, ErrSampleConflict) {
			t.Fatalf("gauge collision accepted: %v", err)
		}
	}
	last, ok, err := st.LastSample(ctx, scope.DeploymentID, name, p.Series)
	if err != nil || !ok || last.Bad != 3 || last.Eligible != 100 {
		t.Fatalf("collision modified original: %+v %v", last, err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM sage.sre_sli_samples WHERE deployment_id=$1 AND slo_name=$2`, string(scope.DeploymentID), name).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestEnginePushScopedRequiresExactDurableIdentity(t *testing.T) {
	f := newEngineFixture(t)
	name := uniqueName("scope")
	e := f.engine(t, []Objective{pushObjective(name)}, nil, nil)
	p := sample("actor:1", 1, 100, time.Now().UTC().Truncate(time.Microsecond))
	for _, scope := range []sre.Scope{{}, {DeploymentID: f.scope.DeploymentID, DatabaseID: sre.NewUUID()}, {DeploymentID: sre.NewUUID(), DatabaseID: f.scope.DatabaseID}} {
		if created, err := e.PushScoped(f.ctx, scope, name, p); created || !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign scope accepted: %v %v", created, err)
		}
	}
	if created, err := e.PushScoped(f.ctx, f.scope, name, p); err != nil || !created {
		t.Fatalf("own scope: %v %v", created, err)
	}
	if created, err := e.PushScoped(f.ctx, f.scope, name, p); err != nil || created {
		t.Fatalf("identical replay: %v %v", created, err)
	}
	p.Bad++
	if _, err := e.PushScoped(f.ctx, f.scope, name, p); !errors.Is(err, ErrSampleConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
}
