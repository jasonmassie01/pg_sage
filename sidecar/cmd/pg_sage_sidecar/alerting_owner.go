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
// the next decision follows it without a restart. Routes and channels
// stay restart-bound.
type alertingOwner struct {
	mu          sync.Mutex
	dispatchers map[*notify.Dispatcher]struct{}
	throttles   map[*alerting.Throttle]struct{}
}

// alertingRuntime is the process's alerting owner.
var alertingRuntime = newAlertingOwner()

func newAlertingOwner() *alertingOwner {
	return &alertingOwner{dispatchers: map[*notify.Dispatcher]struct{}{},
		throttles: map[*alerting.Throttle]struct{}{}}
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
// typo is rejected at apply time with the field named.
func (o *alertingOwner) Prepare(
	_ context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	if desired.Config == nil {
		return nil, fmt.Errorf("alerting: desired config is nil")
	}
	next := desired.Config.Alerting
	if _, warns := alertPolicyOf(next); len(warns) > 0 {
		return nil, fmt.Errorf("alerting.timezone / alerting.quiet_hours_*: %s",
			strings.Join(warns, "; "))
	}
	prev := next
	if active.Config != nil {
		prev = active.Config.Alerting
	}
	return preparedAlerting{owner: o, prev: prev, next: next}, nil
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
	owner      *alertingOwner
	prev, next config.AlertingConfig
}

func (p preparedAlerting) Commit(context.Context) error {
	p.owner.apply(p.next)
	return nil
}

func (p preparedAlerting) Rollback(context.Context) error {
	p.owner.apply(p.prev)
	return nil
}

func (preparedAlerting) Drain(context.Context) error { return nil }

// registerAlertingOwner registers the alerting owner in every mode.
func registerAlertingOwner() {
	if configController == nil {
		return
	}
	if err := configController.RegisterOwner(alertingRuntime); err != nil {
		logWarn("config", "alerting reconfiguration owner: %v", err)
	}
}
