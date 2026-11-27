package main

import (
	"context"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/cost"
)

// market-04: the shared telemetry runtime hands each poll's hourly
// aggregate to every member database, which keeps it in its own
// sage.host_usage_hourly; an hourly worker judges the instance's size from
// it. Both only read the provider and write pg_sage's own tables; the
// verdict is an advisory finding, never an action.

// usageLogEvery repeats an unchanged usage-write failure at most hourly.
const usageLogEvery = time.Hour

// rightsizeTimeout bounds one right-size evaluation.
const rightsizeTimeout = 30 * time.Second

// usageFailureLog logs a failed usage write when its error changes, and
// again at most once per usageLogEvery while it persists.
type usageFailureLog struct {
	mu      sync.Mutex
	last    string
	at      time.Time
	now     func() time.Time
	logWarn func(component, msg string, args ...any)
	scope   string
}

func (l *usageFailureLog) report(database string, u cloudtel.HourlyUsage, err error) {
	l.mu.Lock()
	now := l.now()
	repeat := err.Error() == l.last && now.Sub(l.at) < usageLogEvery
	if !repeat {
		l.last, l.at = err.Error(), now
	}
	l.mu.Unlock()
	if repeat {
		return
	}
	l.logWarn(l.scope, "db %q: host usage of %s for %s not recorded (the poll "+
		"continues): %v", database, u.Resource, u.Hour.Format(time.RFC3339), err)
}

// attachHostUsage stores the instance's hourly usage in this database
// until it stops.
func (rt *databaseRuntime) attachHostUsage() {
	if rt.cloud == nil || rt.spec.Pool == nil {
		return
	}
	pool := rt.spec.Pool
	failures := &usageFailureLog{now: time.Now, logWarn: logWarn, scope: rt.spec.Scope}
	detach := rt.cloud.AttachUsage(rt.spec.Name,
		func(ctx context.Context, u cloudtel.HourlyUsage) error {
			return cost.RecordHourly(ctx, pool, u)
		}, failures.report)
	rt.start(func() {
		<-rt.ctx.Done()
		detach()
	})
}

// startRightsize judges the instance's size every RightsizeInterval.
func (rt *databaseRuntime) startRightsize() {
	if rt.cloud == nil || rt.cloudResolver == nil || rt.spec.Pool == nil || rt.cfg == nil {
		return
	}
	rates := cost.NewConfigRates(rt.cfg.Cost)
	rt.start(func() {
		ticker := time.NewTicker(cost.RightsizeInterval)
		defer ticker.Stop()
		for {
			rt.evaluateRightsize(rates)
			select {
			case <-rt.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	rt.note("instance_rightsize")
}

func (rt *databaseRuntime) evaluateRightsize(rates cost.RateSource) {
	resource := rt.cloud.Resource()
	if resource == "" {
		return // no good poll yet: nothing names the instance
	}
	ctx, cancel := context.WithTimeout(rt.ctx, rightsizeTimeout)
	defer cancel()
	_, err := cost.EvaluateRightsize(ctx, rt.spec.Pool, resource, rt.spec.Name, rates,
		time.Now())
	if err != nil && rt.ctx.Err() == nil {
		logWarn(rt.spec.Scope, "db %q: instance right-sizing of %s: %v", rt.spec.Name,
			resource, err)
	}
}
