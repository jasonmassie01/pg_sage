package pgconf

import (
	"fmt"
	"strings"
)

// advisorReloptions are the table storage parameters the advisor may
// propose as executable SQL, with their safe ranges: autovacuum
// thresholds, scale factors and cost limits, and fillfactor. Disabling
// autovacuum is never on it.
var advisorReloptions = map[string]GUCDoc{
	"autovacuum_vacuum_scale_factor":        Docs["autovacuum_vacuum_scale_factor"],
	"autovacuum_vacuum_threshold":           Docs["autovacuum_vacuum_threshold"],
	"autovacuum_vacuum_insert_scale_factor": Docs["autovacuum_vacuum_insert_scale_factor"],
	"autovacuum_vacuum_insert_threshold":    Docs["autovacuum_vacuum_insert_threshold"],
	"autovacuum_analyze_scale_factor":       Docs["autovacuum_analyze_scale_factor"],
	"autovacuum_analyze_threshold":          Docs["autovacuum_analyze_threshold"],
	"autovacuum_vacuum_cost_delay":          Docs["autovacuum_vacuum_cost_delay"],
	"autovacuum_vacuum_cost_limit":          Docs["autovacuum_vacuum_cost_limit"],
	"fillfactor": {
		Description: "Percentage of each heap page filled on insert; the rest is " +
			"left for HOT updates.",
		Guidance: "90 for update-heavy tables; below 70 wastes space for little gain.",
		Unit:     "count", SafeMin: 50, SafeMax: 100,
	},
}

// toastReloptions are the advisor reloptions PostgreSQL also accepts in
// the "toast." namespace (TOAST tables have no analyze or fillfactor).
var toastReloptions = map[string]bool{
	"autovacuum_vacuum_scale_factor":        true,
	"autovacuum_vacuum_threshold":           true,
	"autovacuum_vacuum_insert_scale_factor": true,
	"autovacuum_vacuum_insert_threshold":    true,
	"autovacuum_vacuum_cost_delay":          true,
	"autovacuum_vacuum_cost_limit":          true,
}

// executorOnlyReloptions are storage parameters the executor may set for
// other producers (custodian freeze response, rollbacks of operator
// changes) but the advisor may not propose. autovacuum_enabled is here
// only so it can be turned back on; false is always refused.
var executorOnlyReloptions = map[string]bool{
	"autovacuum_enabled":                  true,
	"autovacuum_freeze_min_age":           true,
	"autovacuum_freeze_max_age":           true,
	"autovacuum_freeze_table_age":         true,
	"autovacuum_multixact_freeze_min_age": true,
	"autovacuum_multixact_freeze_max_age": true,
}

// advisorReloptionDoc returns the range of an advisor reloption key.
func advisorReloptionDoc(key string) (GUCDoc, bool) {
	base, toast := ReloptionBaseKey(key)
	if toast && !toastReloptions[base] {
		return GUCDoc{}, false
	}
	doc, ok := advisorReloptions[base]
	return doc, ok
}

// AdvisorReloption reports whether the advisor may propose the key.
func AdvisorReloption(key string) bool {
	_, ok := advisorReloptionDoc(key)
	return ok
}

// ValidateReloption range-checks a reloption value. Keys outside the
// advisor allowlist pass here; the allowlist decides whether they run.
func ValidateReloption(key, value string) (bool, string) {
	doc, ok := advisorReloptionDoc(key)
	if !ok {
		return true, ""
	}
	return validateDoc(key, value, doc)
}

// CheckExecutableReloption is the executor's reloption rule: the key must
// be allowlisted, and autovacuum may never be switched off. A RESET
// restores the default and is allowed for every known key.
func CheckExecutableReloption(opt Reloption, reset bool) error {
	base, toast := ReloptionBaseKey(opt.Key)
	known := AdvisorReloption(opt.Key) || executorOnlyReloptions[base] &&
		(!toast || base == "autovacuum_enabled")
	if !known {
		return fmt.Errorf("%w: %q is not an allowlisted storage parameter",
			ErrReloptionRefused, opt.Key)
	}
	if !reset && base == "autovacuum_enabled" && !boolTrue(opt.Value) {
		return fmt.Errorf("%w: %s=%s would disable autovacuum",
			ErrReloptionRefused, opt.Key, opt.Value)
	}
	return nil
}

// boolTrue reports whether PostgreSQL parses v as boolean true (unique
// prefixes of true/yes, "on", "1").
func boolTrue(v string) bool {
	s := strings.ToLower(strings.TrimSpace(Unquote(v)))
	switch {
	case s == "":
		return false
	case strings.HasPrefix("true", s), strings.HasPrefix("yes", s):
		return true
	case s == "on", s == "1":
		return true
	}
	return false
}
