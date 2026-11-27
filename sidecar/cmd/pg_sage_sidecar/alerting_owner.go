package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alertingOwner is the "alerting" reconfiguration owner (alerting-06): a
// live change of the cooldown, quiet hours, timezone or repeat intervals
// reaches every outbox dispatcher and every YAML manager's throttle, so
// the next decision follows it without a restart. With the notify engine
// a change of the routes and targets is reconciled into every control
// database's notification tables (alerting-06b).
type alertingOwner struct {
	mu          sync.Mutex
	dispatchers map[*notify.Dispatcher]struct{}
	throttles   map[*alerting.Throttle]struct{}
	reconcilers map[notify.ReconcileStore]struct{}
}

// alertingRuntime is the process's alerting owner.
var alertingRuntime = newAlertingOwner()

func newAlertingOwner() *alertingOwner {
	return &alertingOwner{dispatchers: map[*notify.Dispatcher]struct{}{},
		throttles:   map[*alerting.Throttle]struct{}{},
		reconcilers: map[notify.ReconcileStore]struct{}{}}
}

func (o *alertingOwner) Name() string { return config.AlertingOwner }

func (o *alertingOwner) addDispatcher(d *notify.Dispatcher) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dispatchers[d] = struct{}{}
}

func (o *alertingOwner) addThrottle(t *alerting.Throttle) {
	if t == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.throttles[t] = struct{}{}
}

// Prepare refuses a timezone or quiet hours that would be disabled, so a
// typo is rejected at apply time with the field named, and reconciles a
// change of the routes or targets; a reconcile that fails refuses the
// change and leaves the tables as they were.
func (o *alertingOwner) Prepare(
	ctx context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	if desired.Config == nil {
		return nil, fmt.Errorf("alerting: desired config is nil")
	}
	next := desired.Config.Alerting
	if _, warns := alertPolicyOf(next); len(warns) > 0 {
		return nil, fmt.Errorf("alerting.timezone / alerting.quiet_hours_*: %s",
			strings.Join(warns, "; "))
	}
	prevCfg := desired.Config
	if active.Config != nil {
		prevCfg = active.Config
	}
	if err := o.reconcileChange(ctx, prevCfg, desired.Config); err != nil {
		return nil, err
	}
	return preparedAlerting{owner: o, prevCfg: prevCfg, nextCfg: desired.Config}, nil
}

// apply gives every tracked dispatcher and throttle a's settings.
func (o *alertingOwner) apply(a config.AlertingConfig) {
	policy, _ := alertPolicyOf(a)
	o.mu.Lock()
	defer o.mu.Unlock()
	for d := range o.dispatchers {
		d.SetAlertPolicy(policy)
	}
	for t := range o.throttles {
		t.Reconfigure(a.CooldownMinutes, a.RepeatMinutes, a.QuietHoursStart,
			a.QuietHoursEnd, a.Timezone)
	}
}

func alertPolicyOf(a config.AlertingConfig) (notify.AlertPolicy, []string) {
	return notify.NewAlertPolicy(a.CooldownMinutes, a.RepeatMinutes, a.QuietHoursStart,
		a.QuietHoursEnd, a.Timezone)
}

type preparedAlerting struct {
	owner            *alertingOwner
	prevCfg, nextCfg *config.Config
}

func (p preparedAlerting) Commit(context.Context) error {
	p.owner.apply(p.nextCfg.Alerting)
	return nil
}

// Rollback restores the policy and reconciles the previous block back.
func (p preparedAlerting) Rollback(ctx context.Context) error {
	p.owner.apply(p.prevCfg.Alerting)
	return p.owner.reconcileChange(ctx, p.nextCfg, p.prevCfg)
}

func (preparedAlerting) Drain(context.Context) error { return nil }

// registerAlertingOwner registers the alerting owner in every mode.
func registerAlertingOwner() {
	warnLegacyAlertingEngine(cfg)
	if configController == nil {
		return
	}
	if err := configController.RegisterOwner(alertingRuntime); err != nil {
		logWarn("config", "alerting reconfiguration owner: %v", err)
	}
}
