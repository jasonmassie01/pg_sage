package managedparam

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
)

const (
	// DriftPendingReboot: the group/flag holds a value PostgreSQL will
	// only run after the next reboot.
	DriftPendingReboot = "pending_reboot"
	// DriftOverridden: a database, role or session setting overrides it.
	DriftOverridden = "overridden"
	// DriftMismatch: the configured value is not running and nothing
	// explains why (a console edit that never took effect, an
	// unattached group).
	DriftMismatch = "mismatch"
)

// Drift is one parameter whose configured provider value differs from
// what PostgreSQL runs.
type Drift struct {
	Parameter  string `json:"parameter"`
	Configured string `json:"configured"`
	Running    string `json:"running"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail"`
}

// DetectDrift compares the operator-set parameters of a resolved target
// (RDS "user" parameters, Cloud SQL flags) with pg_settings. Formulas,
// engine defaults and parameters PostgreSQL does not expose are skipped.
func DetectDrift(target Target, settings []collector.PGSetting) []Drift {
	if !target.Known {
		return nil
	}
	running := make(map[string]collector.PGSetting, len(settings))
	for _, s := range settings {
		running[s.Name] = s
	}
	var out []Drift
	for name, configured := range configuredValues(target) {
		s, ok := running[name]
		if !ok || strings.HasPrefix(configured, "{") || sameValue(configured, s.Setting) {
			continue
		}
		out = append(out, classify(name, configured, s, target))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Parameter < out[j].Parameter })
	return out
}

func configuredValues(t Target) map[string]string {
	out := map[string]string{}
	if NormalizeProvider(t.Provider) == "cloud-sql" {
		if t.FlagsKnown {
			for k, v := range t.Flags {
				out[k] = v
			}
		}
		return out
	}
	for k, gp := range t.Params {
		if gp.Source == "user" && gp.Value != "" {
			out[k] = gp.Value
		}
	}
	return out
}

func classify(name, configured string, s collector.PGSetting, t Target) Drift {
	d := Drift{Parameter: name, Configured: configured, Running: s.Setting}
	switch {
	case s.PendingRestart || t.ParameterGroupStatus == "pending-reboot":
		d.Kind = DriftPendingReboot
		d.Detail = fmt.Sprintf("%s is set to %s but %s runs until the next reboot", name,
			configured, s.Setting)
	case s.Source != "" && s.Source != "configuration file" && s.Source != "default":
		d.Kind = DriftOverridden
		d.Detail = fmt.Sprintf("%s is set to %s but a %s-level setting overrides it with %s",
			name, configured, s.Source, s.Setting)
	default:
		d.Kind = DriftMismatch
		d.Detail = fmt.Sprintf("%s is set to %s but PostgreSQL runs %s: the change did not "+
			"take effect", name, configured, s.Setting)
	}
	return d
}

// sameValue compares numerically when both parse, as booleans when both
// are booleans, else case-insensitively.
func sameValue(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if x, err := strconv.ParseFloat(a, 64); err == nil {
		if y, err := strconv.ParseFloat(b, 64); err == nil {
			return x == y
		}
	}
	if x, ok := boolValue(a); ok {
		if y, ok := boolValue(b); ok {
			return x == y
		}
	}
	return strings.EqualFold(a, b)
}

func boolValue(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return false, false
}
