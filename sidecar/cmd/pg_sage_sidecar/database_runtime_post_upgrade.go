package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// postUpgradeTicker runs the post-upgrade producer (drill-02c) before each
// plan engine tick: a major version change on the change feed becomes a
// proposed plan, which the engine runs only after a person approves it.
// Only the fleet leader proposes; a producer failure is logged and the
// engine still ticks (the change stays on the feed, so it is retried).
type postUpgradeTicker struct {
	name     string
	producer interface {
		Tick(ctx context.Context) (upgradecheck.PostUpgradeResult, error)
	}
	leader func() bool
	next   planTicker
}

func (t postUpgradeTicker) Tick(ctx context.Context) error {
	if t.leader() {
		if _, err := t.producer.Tick(ctx); err != nil {
			logWarn("plan", "[%s] post-upgrade plans: %v", t.name, err)
		}
	}
	return t.next.Tick(ctx)
}

// withPostUpgrade wraps the database's plan engine with the post-upgrade
// producer. It needs the change feed (guard-02b's version changes), which
// runs behind the SRE read policy; without the feed or the engine it adds
// nothing.
func (rt *databaseRuntime) withPostUpgrade(engine planTicker) planTicker {
	if engine == nil || rt.changeFeed == nil {
		return engine
	}
	producer, err := upgradecheck.NewPostUpgrade(upgradecheck.PostUpgradeOptions{
		Pool: rt.spec.Pool, Events: rt.changeFeed,
		Plans:    changeplan.NewStore(rt.spec.Pool).WithCaps(rt.planCaps()),
		Resolver: changeplan.NewDBResolver(rt.spec.Pool, rt.spec.Pool),
		Logf:     logStructuredWrapper})
	if err != nil {
		logError(rt.spec.Scope, "db %q: post-upgrade plans unavailable: %v", rt.spec.Name, err)
		return engine
	}
	rt.note("post_upgrade_plans")
	return postUpgradeTicker{name: rt.spec.Name, producer: producer, next: engine,
		leader: func() bool { return fleetLeader == nil || fleetLeader.IsLeader() }}
}
