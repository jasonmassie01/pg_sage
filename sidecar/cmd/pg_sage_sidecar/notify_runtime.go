package main

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

var (
	notifyDispatchersMu sync.Mutex
	notifyDispatchers   = map[*pgxpool.Pool]*notify.Dispatcher{}
	notifyStores        = map[*pgxpool.Pool]*notify.PoolStore{}
)

// sharedNotifyStore is the store of the dispatcher bound to controlPool,
// or nil before sharedNotifyDispatcher built it.
func sharedNotifyStore(controlPool *pgxpool.Pool) *notify.PoolStore {
	notifyDispatchersMu.Lock()
	defer notifyDispatchersMu.Unlock()
	return notifyStores[controlPool]
}

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
	store := notify.NewPoolStore(controlPool, notificationSecretKey(controlPool)).
		WithPreviousKey(notificationPreviousSecretKey(controlPool))
	dispatcher := notify.NewDefaultDispatcher(store, notificationTargetPolicy(),
		logStructuredWrapper)
	notifyStores[controlPool] = store
	startNotifyOutbox(dispatcher, store)
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

// notificationPreviousSecretKey is the previous key of a rotation for
// channel secrets stored in controlPool, derived like notificationSecretKey
// (security-08), so readers open values the startup re-seal did not move
// (YAML fleet mode has none). nil without a rotation or a key.
func notificationPreviousSecretKey(controlPool *pgxpool.Pool) []byte {
	keys, err := notificationKeys(context.Background(), controlPool)
	if err != nil || keys == nil {
		return nil
	}
	return keys.previous
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
	return configTargetPolicy(cfg)
}

// newInstanceAlertManager builds the alerting manager for one monitored
// database, or nil when alerting is disabled. Fleet and meta instances were
// silently never alerted (G7-B09). name is stamped on every alert, so
// payloads say which database they concern and PagerDuty keys stay apart
// across a fleet (alerting-02).
//
// Only the legacy engine has one (alerting-06b): with the default notify
// engine the alerting: block is reconciled into notification channels and
// delivered by the outbox, so a second manager would post twice.
func newInstanceAlertManager(pool *pgxpool.Pool, name string) *alerting.Manager {
	if pool == nil || cfg == nil || !cfg.Alerting.Enabled ||
		cfg.Alerting.EffectiveEngine() != config.AlertingEngineLegacy {
		return nil
	}
	routes := buildAlertRoutes(cfg, logStructuredWrapper)
	m := alerting.New(pool, alerting.ManagerConfig{
		CheckIntervalSeconds: cfg.Alerting.CheckIntervalSeconds,
		CooldownMinutes:      cfg.Alerting.CooldownMinutes,
		RepeatMinutes:        cfg.Alerting.RepeatMinutes,
		QuietHoursStart:      cfg.Alerting.QuietHoursStart,
		QuietHoursEnd:        cfg.Alerting.QuietHoursEnd,
		Timezone:             cfg.Alerting.Timezone,
		DatabaseName:         name,
	}, routes, logStructuredWrapper)
	alertingRuntime.addThrottle(m.Throttle())
	return m
}

// notifyOutboxInterval is how often the outbox is drained.
const notifyOutboxInterval = 5 * time.Second

// startNotifyOutbox queues the dispatcher's rule deliveries in the control
// database's outbox, under the alerting.* cooldown, quiet hours and repeat
// intervals, and drains it until shutdown (alerting-06). The outbox needs
// the process's lifetime to drain: before the sidecar has one (unit tests
// of this package) the dispatcher keeps delivering at once.
func startNotifyOutbox(d *notify.Dispatcher, store *notify.PoolStore) {
	if shutdownCtx == nil {
		return
	}
	d.UseOutbox(store)
	d.SetAlertPolicy(notifyAlertPolicy(cfg))
	alertingRuntime.addDispatcher(d)
	alertingRuntime.reconcileAtBoot(shutdownCtx, store)
	go d.RunOutbox(shutdownCtx, notifyOutboxInterval)
}

// notifyAlertPolicy is the alert policy of c's alerting block; invalid
// values (refused at startup when alerting is enabled) are logged.
func notifyAlertPolicy(c *config.Config) notify.AlertPolicy {
	if c == nil {
		p, _ := notify.NewAlertPolicy(0, nil, "", "", "")
		return p
	}
	p, warns := alertPolicyOf(c.Alerting)
	for _, w := range warns {
		logWarn("notify", "%s", w)
	}
	return p
}
