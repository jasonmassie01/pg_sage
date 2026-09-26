package main

import (
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/notify"
)

var (
	notifyDispatchersMu sync.Mutex
	notifyDispatchers   = map[*pgxpool.Pool]*notify.Dispatcher{}
)

// notificationControlPool is the database the UI writes notification
// channels and rules to: the meta DB, else the fleet primary (auth) pool.
// Per-database dispatchers used to read rules from each monitored DB, so
// rules never matched outside the primary (G5-B10, G7-B05).
func notificationControlPool(
	meta *metaDBState, primary *pgxpool.Pool,
) *pgxpool.Pool {
	if meta != nil && meta.Pool != nil {
		return meta.Pool
	}
	return primary
}

// sharedNotifyDispatcher returns the one dispatcher bound to a control pool;
// every instance shares it and tags events with its own database name.
func sharedNotifyDispatcher(controlPool *pgxpool.Pool) *notify.Dispatcher {
	if controlPool == nil {
		return nil
	}
	notifyDispatchersMu.Lock()
	defer notifyDispatchersMu.Unlock()
	if dispatcher := notifyDispatchers[controlPool]; dispatcher != nil {
		return dispatcher
	}
	dispatcher := notify.NewDispatcher(controlPool, logStructuredWrapper)
	registerNotifySenders(dispatcher)
	notifyDispatchers[controlPool] = dispatcher
	return dispatcher
}

// newInstanceAlertManager builds the alerting manager for one monitored
// database, or nil when alerting is disabled. Fleet and meta instances were
// silently never alerted (G7-B09).
func newInstanceAlertManager(pool *pgxpool.Pool) *alerting.Manager {
	if pool == nil || !cfg.Alerting.Enabled {
		return nil
	}
	routes := buildAlertRoutes(cfg, logStructuredWrapper)
	return alerting.New(pool, alerting.ManagerConfig{
		CheckIntervalSeconds: cfg.Alerting.CheckIntervalSeconds,
		CooldownMinutes:      cfg.Alerting.CooldownMinutes,
		QuietHoursStart:      cfg.Alerting.QuietHoursStart,
		QuietHoursEnd:        cfg.Alerting.QuietHoursEnd,
		Timezone:             cfg.Alerting.Timezone,
	}, routes, logStructuredWrapper)
}
