package pgconf

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrReloptionRefused marks a storage parameter the executor never sets.
var ErrReloptionRefused = errors.New("storage parameter refused")

// executableGUCs is the executor's whitelist: the only settings any path
// (advisor, custodian, operator approval, rollback) may ALTER SYSTEM or
// ALTER DATABASE. Every advisor GUC (Docs) must be in it.
var executableGUCs = map[string]bool{
	"work_mem":                              true,
	"maintenance_work_mem":                  true,
	"effective_cache_size":                  true,
	"shared_buffers":                        true,
	"max_wal_size":                          true,
	"min_wal_size":                          true,
	"max_slot_wal_keep_size":                true,
	"checkpoint_completion_target":          true,
	"checkpoint_timeout":                    true,
	"random_page_cost":                      true,
	"effective_io_concurrency":              true,
	"max_parallel_workers_per_gather":       true,
	"max_parallel_workers":                  true,
	"max_parallel_maintenance_workers":      true,
	"autovacuum_vacuum_cost_delay":          true,
	"autovacuum_vacuum_cost_limit":          true,
	"autovacuum_naptime":                    true,
	"autovacuum_max_workers":                true,
	"autovacuum_vacuum_threshold":           true,
	"autovacuum_vacuum_scale_factor":        true,
	"autovacuum_vacuum_insert_threshold":    true,
	"autovacuum_vacuum_insert_scale_factor": true,
	"autovacuum_analyze_threshold":          true,
	"autovacuum_analyze_scale_factor":       true,
	"wal_buffers":                           true,
	"default_statistics_target":             true,
	"huge_pages":                            true,
	"temp_buffers":                          true,
	"log_min_duration_statement":            true,
	"track_activity_query_size":             true,
	"jit":                                   true,
	"max_connections":                       true,
}

func norm(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ExecutableGUC reports whether any executor path may change the setting.
func ExecutableGUC(name string) bool { return executableGUCs[norm(name)] }

// AdvisorGUC reports whether the advisor may propose the setting as
// executable SQL (it is documented with a safe range).
func AdvisorGUC(name string) bool {
	_, ok := Docs[norm(name)]
	return ok
}

// AutonomousGUC reports whether pg_sage may change the setting without an
// operator: documented, ranged, and live after a reload.
func AutonomousGUC(name string) bool {
	return AdvisorGUC(name) && !RequiresRestart(name)
}

// ValidateValue reports whether a proposed value is within the documented
// safe range. Undocumented settings pass here: whether they may be
// changed at all is the allowlists' decision, not the range check's.
func ValidateValue(name, value string) (ok bool, reason string) {
	doc, known := Docs[norm(name)]
	if !known {
		return true, ""
	}
	return validateDoc(name, value, doc)
}

func validateDoc(name, value string, doc GUCDoc) (bool, string) {
	if isSentinel(value, doc) {
		return true, ""
	}
	v, err := ParseValue(value, doc)
	if err != nil {
		return false, fmt.Sprintf("unparseable value %q for %s: %v", value, name, err)
	}
	if v < doc.SafeMin || v > doc.SafeMax {
		return false, fmt.Sprintf("%s=%s is outside the safe range %s",
			name, value, RangeString(doc))
	}
	return true, ""
}

// isSentinel reports whether a unitless value is one of the setting's
// special values (compared before base-unit scaling).
func isSentinel(value string, doc GUCDoc) bool {
	raw := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `'"`))
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return false
	}
	for _, sentinel := range doc.Sentinels {
		if n == sentinel {
			return true
		}
	}
	return false
}
