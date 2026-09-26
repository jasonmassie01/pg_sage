package main

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/schema"
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
	notifyDispatchers[controlPool] = dispatcher
	return dispatcher
}

// notificationSecretKey is the key that seals notification channel
// secrets stored in controlPool (G7-B20): the meta-db encryption key, or
// in standalone / YAML fleet the same passphrase derived with that
// database's persisted KDF salt. The API router and every dispatcher call
// this with the same pool, so writes and reads agree. nil means secrets
// stay plaintext (no passphrase, or a pool that is neither the meta pool
// nor a standalone/fleet control database).
func notificationSecretKey(controlPool *pgxpool.Pool) []byte {
	if controlPool == nil || cfg == nil {
		return nil
	}
	if globalMetaState != nil && globalMetaState.Pool == controlPool {
		return globalMetaState.EncryptKey
	}
	if cfg.EncryptionKey == "" || (!cfg.IsStandalone() && !cfg.IsFleet()) {
		return nil
	}
	salt, err := schema.ReadOrCreateKDFSalt(context.Background(), controlPool)
	if err != nil {
		logError("notify", "channel secrets stay unsealed: KDF salt unavailable: %v", err)
		return nil
	}
	return crypto.DeriveKey(cfg.EncryptionKey, salt)
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
