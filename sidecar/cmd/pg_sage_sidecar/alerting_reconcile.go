package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// The alerting owner's reconcile side (alerting-06b): with the notify
// engine the config file's alerting: block is reconciled into the
// notification tables of every control database at boot and on every
// live change of its routes and targets, so one engine delivers it.

// addReconciler tracks a control database's store for live changes.
func (o *alertingOwner) addReconciler(s notify.ReconcileStore) {
	if s == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reconcilers[s] = struct{}{}
}

func (o *alertingOwner) reconcileStores() []notify.ReconcileStore {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]notify.ReconcileStore, 0, len(o.reconcilers))
	for s := range o.reconcilers {
		out = append(out, s)
	}
	return out
}

// reconcileAtBoot tracks s and reconciles the running config's block into
// it. With the legacy engine the block is reconciled as absent: the
// legacy manager delivers it, so config rows left by an earlier notify
// run would post every finding a second time. It returns what was
// written, or nil when nothing ran or the reconcile failed (logged:
// config alerts are not delivered until a later reconcile succeeds).
func (o *alertingOwner) reconcileAtBoot(
	ctx context.Context, s notify.ReconcileStore,
) *notify.ReconcileResult {
	if cfg == nil || s == nil {
		return nil
	}
	o.addReconciler(s)
	block := cfg.Alerting
	if block.EffectiveEngine() == config.AlertingEngineLegacy {
		block = config.AlertingConfig{}
	}
	res, err := notify.Reconcile(ctx, s, block, configTargetPolicy(cfg),
		logStructuredWrapper)
	if err != nil {
		logError("alerting", "reconciling the alerting: block into notification channels "+
			"failed; its alerts are not delivered until a reconcile succeeds: %v", err)
		return nil
	}
	if res != (notify.ReconcileResult{}) {
		logInfo("alerting", "alerting: block reconciled into notification channels: %+v", res)
	}
	return &res
}

// alertingTargetsChanged reports whether a change touches what the
// reconciler maps: the switch, the routes or a target.
func alertingTargetsChanged(prev, next config.AlertingConfig) bool {
	return prev.Enabled != next.Enabled || prev.SlackWebhookURL != next.SlackWebhookURL ||
		prev.PagerDutyRoutingKey != next.PagerDutyRoutingKey ||
		!reflect.DeepEqual(prev.Routes, next.Routes) ||
		!reflect.DeepEqual(prev.Webhooks, next.Webhooks)
}

// reconcileChange applies a live change of the block's routes or targets.
// The running engine decides: legacy builds its routes at startup only,
// so the change is refused rather than left silently unapplied. A store
// that fails restores the stores already reconciled to prev.
func (o *alertingOwner) reconcileChange(ctx context.Context, prev, next *config.Config) error {
	if !alertingTargetsChanged(prev.Alerting, next.Alerting) {
		return nil
	}
	if prev.Alerting.EffectiveEngine() == config.AlertingEngineLegacy {
		return fmt.Errorf("alerting.engine is legacy: a change of alerting.enabled, routes, " +
			"webhooks or the Slack and PagerDuty targets applies only after a restart; " +
			"restart the sidecar, or set alerting.engine: notify")
	}
	stores := o.reconcileStores()
	for i, s := range stores {
		if _, err := notify.Reconcile(ctx, s, next.Alerting, configTargetPolicy(next),
			logStructuredWrapper); err != nil {
			restoreAlerting(ctx, stores[:i], prev)
			return fmt.Errorf("alerting: %w", err)
		}
	}
	return nil
}

// restoreAlerting reconciles stores back to prev after a failed change.
func restoreAlerting(ctx context.Context, stores []notify.ReconcileStore, prev *config.Config) {
	for _, s := range stores {
		if _, err := notify.Reconcile(ctx, s, prev.Alerting, configTargetPolicy(prev),
			logStructuredWrapper); err != nil {
			logError("alerting", "restoring the previous alerting: block failed: %v", err)
		}
	}
}

// warnLegacyAlertingEngine logs, once at startup, that the legacy engine
// is the escape hatch of one release.
func warnLegacyAlertingEngine(c *config.Config) {
	if c == nil || c.Alerting.EffectiveEngine() != config.AlertingEngineLegacy {
		return
	}
	logWarn("alerting", "alerting.engine is legacy: the old per-database alerting "+
		"manager delivers the alerting: block, nothing is reconciled into notification "+
		"channels and config-managed channels left by the notify engine are removed. "+
		"The legacy engine is removed by alerting-06d; switch to "+
		"alerting.engine: notify (the default)")
}
