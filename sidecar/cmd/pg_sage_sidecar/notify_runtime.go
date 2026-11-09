package main

import (
	"context"
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
	dispatcher := notify.NewDispatcherWithStore(
		notify.NewPoolStore(controlPool, notificationSecretKey(controlPool)),
		logStructuredWrapper,
	)
	registerNotifySenders(dispatcher)
	withApprovalCardTokens(dispatcher, controlPool)
	withFactCardTokens(dispatcher, controlPool)
	notifyDispatchers[controlPool] = dispatcher
	return dispatcher
}

// notificationSecretKey is the key that seals notification channel
// secrets stored in controlPool (G7-B20): the meta-db encryption key, or
// in standalone / YAML fleet the same passphrase (or generated key)
// derived with that database's persisted KDF salt. The API router and
// every dispatcher call this with the same pool, so writes and reads
// agree. nil means secrets stay plaintext (no key, or a pool that is
// neither the meta pool nor a standalone/fleet control database).
func notificationSecretKey(controlPool *pgxpool.Pool) []byte {
	keys, err := notificationKeys(context.Background(), controlPool)
	if err != nil {
		logError("notify", "channel secrets stay unsealed: %v", err)
		return nil
	}
	if keys == nil {
		return nil
	}
	return keys.active
}

// notificationKeys is the key set behind notificationSecretKey.
func notificationKeys(
	ctx context.Context, controlPool *pgxpool.Pool,
) (*controlKeySet, error) {
	if controlPool == nil || cfg == nil {
		return nil, nil
	}
	if globalMetaState != nil && globalMetaState.Pool == controlPool {
		return globalMetaState.Keys, nil
	}
	if !cfg.IsStandalone() && !cfg.IsFleet() {
		return nil, nil
	}
	return controlKeys(ctx, controlPool)
}

// notificationTargetPolicy is the operator's notification target policy;
// private networks are refused unless explicitly allowed (G7-B21).
func notificationTargetPolicy() notify.TargetPolicy {
	return notify.TargetPolicy{
		AllowPrivate: cfg != nil && cfg.NotificationPolicy.AllowPrivateTargets,
	}
}

// newInstanceAlertManager builds the alerting manager for one monitored
// database, or nil when alerting is disabled. Fleet and meta instances were
// silently never alerted (G7-B09). name is stamped on every alert, so
// payloads say which database they concern and PagerDuty keys stay apart
// across a fleet (alerting-02).
func newInstanceAlertManager(pool *pgxpool.Pool, name string) *alerting.Manager {
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
		DatabaseName:         name,
	}, routes, logStructuredWrapper)
}
