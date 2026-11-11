package config

import (
	"fmt"
	"sort"
)

// MaxAlertRepeatMinutes bounds alerting.repeat_minutes at one year.
const MaxAlertRepeatMinutes = 525600

// alertCooldownFloors are the throttle's per-severity cooldown floors in
// minutes; any other severity takes the info floor.
var alertCooldownFloors = map[string]int{"critical": 5, "warning": 30, "info": 360}

// AlertCooldownFloorMinutes is the cooldown the alert throttle applies to
// severity: its floor (critical 5, warning 30, info and others 360),
// raised by alerting.cooldown_minutes.
func AlertCooldownFloorMinutes(severity string, cooldownMinutes int) int {
	floor, ok := alertCooldownFloors[severity]
	if !ok {
		floor = alertCooldownFloors["info"]
	}
	if cooldownMinutes > floor {
		return cooldownMinutes
	}
	return floor
}

// ResolveAlertRepeatMinutes returns a new map holding every severity's
// repeat interval: the value in minutes when present, else its default
// (critical 240, warning 1440, info 0). The input is never modified.
func ResolveAlertRepeatMinutes(minutes map[string]int) map[string]int {
	out := map[string]int{
		"critical": DefaultAlertingRepeatCriticalMinutes,
		"warning":  DefaultAlertingRepeatWarningMinutes,
		"info":     DefaultAlertingRepeatInfoMinutes,
	}
	for sev, v := range minutes {
		out[sev] = v
	}
	return out
}

// EffectiveRepeatMinutes is the block's repeat intervals with defaults
// filled in for missing severities.
func (a *AlertingConfig) EffectiveRepeatMinutes() map[string]int {
	return ResolveAlertRepeatMinutes(a.RepeatMinutes)
}

// validateRepeat refuses an unknown severity, a negative interval, one
// above a year, or one below the severity's cooldown (alerting-07).
func (a *AlertingConfig) validateRepeat() error {
	sevs := make([]string, 0, len(a.RepeatMinutes))
	for sev := range a.RepeatMinutes {
		sevs = append(sevs, sev)
	}
	sort.Strings(sevs)
	for _, sev := range sevs {
		v, key := a.RepeatMinutes[sev], "alerting.repeat_minutes."+sev
		floor := AlertCooldownFloorMinutes(sev, a.CooldownMinutes)
		switch {
		case !alertSeverities[sev]:
			return fmt.Errorf("%s: severity %q is not one of critical, warning, info",
				key, sev)
		case v < 0 || v > MaxAlertRepeatMinutes:
			return fmt.Errorf("%s is %d; use 0 (never remind) or 1-%d minutes",
				key, v, MaxAlertRepeatMinutes)
		case v > 0 && v < floor:
			return fmt.Errorf("%s is %d, below the %s cooldown of %d minutes "+
				"(alerting.cooldown_minutes raises it); use 0 (never remind) or at "+
				"least %d", key, v, sev, floor, floor)
		}
	}
	return nil
}

func cloneIntMap(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
