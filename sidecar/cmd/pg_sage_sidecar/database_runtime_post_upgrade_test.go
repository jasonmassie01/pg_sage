package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"

	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// drill-02c wiring: the producer runs before the plan engine, on the
// leader only, and its failure never stops the engine's tick. No
// concurrent-access test: the ticker holds no state of its own.

type fakeProducer struct {
	calls int
	err   error
}

func (f *fakeProducer) Tick(context.Context) (upgradecheck.PostUpgradeResult, error) {
	f.calls++
	return upgradecheck.PostUpgradeResult{}, f.err
}

type fakePlanTicker struct {
	calls int
	err   error
}

func (f *fakePlanTicker) Tick(context.Context) error {
	f.calls++
	return f.err
}

func TestPostUpgradeTicker_ProducerThenEngineOnTheLeader(t *testing.T) {
	prod, eng := &fakeProducer{}, &fakePlanTicker{}
	tk := postUpgradeTicker{name: "db", producer: prod, next: eng,
		leader: func() bool { return true }}
	if err := tk.Tick(context.Background()); err != nil || prod.calls != 1 || eng.calls != 1 {
		t.Fatalf("tick = %v, producer %d, engine %d; want both once", err, prod.calls,
			eng.calls)
	}
	prod.err = errors.New("feed down")
	if err := tk.Tick(context.Background()); err != nil || eng.calls != 2 {
		t.Fatalf("a producer failure = %v, engine %d; want the engine to tick anyway", err,
			eng.calls)
	}
	eng.err = errors.New("engine failed")
	if err := tk.Tick(context.Background()); !errors.Is(err, eng.err) {
		t.Fatalf("engine failure = %v, want it returned", err)
	}
}

func TestPostUpgradeTicker_FollowerDoesNotPropose(t *testing.T) {
	prod, eng := &fakeProducer{}, &fakePlanTicker{}
	tk := postUpgradeTicker{name: "db", producer: prod, next: eng,
		leader: func() bool { return false }}
	if err := tk.Tick(context.Background()); err != nil || prod.calls != 0 || eng.calls != 1 {
		t.Fatalf("follower tick = %v, producer %d, engine %d; want the engine only", err,
			prod.calls, eng.calls)
	}
}

func TestWithPostUpgrade_NeedsTheEngineAndTheFeed(t *testing.T) {
	rt := &databaseRuntime{}
	if got := rt.withPostUpgrade(nil); got != nil {
		t.Fatalf("no engine = %v, want nil", got)
	}
	eng := &fakePlanTicker{}
	if got := rt.withPostUpgrade(eng); got != planTicker(eng) {
		t.Fatalf("no change feed = %T, want the engine unchanged", got)
	}
}

func TestWithPostUpgrade_WrapsTheEngineWhenTheFeedRuns(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u@127.0.0.1:1/x") // lazy
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rt := &databaseRuntime{changeFeed: changefeed.NewFeed(nil, "db", nil)}
	rt.spec.Name = "db"
	eng := &fakePlanTicker{}
	if got := rt.withPostUpgrade(eng); got != planTicker(eng) {
		t.Fatalf("no pool = %T, want the engine unchanged (the producer cannot start)", got)
	}
	rt.spec.Pool = pool
	got, ok := rt.withPostUpgrade(eng).(postUpgradeTicker)
	if !ok || got.next != planTicker(eng) || got.producer == nil || got.name != "db" {
		t.Fatalf("with the feed = %+v, want the engine wrapped by the producer", got)
	}
	if !slicesContain(rt.features, "post_upgrade_plans") {
		t.Fatalf("features = %v, want post_upgrade_plans noted", rt.features)
	}
}

func slicesContain(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestAnalyzeMaxTableBytes_IsTheTunerBoundInBytes(t *testing.T) {
	rt := &databaseRuntime{}
	if got := rt.analyzeMaxTableBytes(); got != config.DefaultTunerAnalyzeMaxTableMB<<20 {
		t.Fatalf("no config = %d, want the default %d MB in bytes", got,
			config.DefaultTunerAnalyzeMaxTableMB)
	}
	rt.cfg = &config.Config{}
	rt.cfg.Tuner.AnalyzeMaxTableMB = 3
	if got := rt.analyzeMaxTableBytes(); got != 3<<20 {
		t.Fatalf("3 MB = %d bytes, want %d", got, 3<<20)
	}
	rt.cfg.Tuner.AnalyzeMaxTableMB = 0
	if got := rt.analyzeMaxTableBytes(); got != 0 {
		t.Fatalf("0 MB = %d, want 0 (no bound, as the executor reads it)", got)
	}
}
