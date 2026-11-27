package findingcat

import (
	"errors"
	"fmt"
	"strings"
)

// Rule is one analyzer snapshot rule and the categories it owns: the
// vocabulary of a rule pack (analyzer.disabled_rules, market-11). The
// analyzer's AllRules must match it (TestAnalyzerRulesMatchAllRules).
type Rule struct {
	Name       string
	Categories []string
}

var analyzerRules = []Rule{
	{"unused_indexes", k("unused_index")},
	{"invalid_indexes", k("invalid_index")},
	{"duplicate_indexes", k("duplicate_index")},
	{"missing_fk_indexes", k("missing_fk_index")},
	{"index_bloat", k("index_bloat")},
	{"table_bloat", k("table_bloat")},
	{"autovacuum_tuning", k("autovacuum_tuning")},
	{"stale_statistics", k("stale_statistics")},
	{"wraparound_freeze", k("wraparound_freeze")},
	{"slow_queries", k("slow_query")},
	{"high_plan_time", k("high_plan_time")},
	{"temp_spills", k("temp_spill")},
	{"cache_hit_ratio", k("cache_hit_ratio")},
	{"checkpoint_pressure", k("checkpoint_pressure")},
	{"stat_statements_capacity", k("stat_statements_pressure")},
	{"high_total_time", k("high_total_time")},
	{"sequence_exhaustion", k("sequence_exhaustion")},
	{"replication_lag", k("replication_lag")},
	{"inactive_slots", k("inactive_slot", "slow_replication_slot")},
	{"subscription_errors", k("subscription_errors")},
	{"subscription_lag", k("subscription_lag")},
	{"cdc_consumer_stalled", k("cdc_consumer_stalled")},
	{"cdc_heartbeat_stale", k("cdc_heartbeat_stale")},
	{"wal_archive_failing", k("wal_archive_failing")},
	{"wal_archive_stale", k("wal_archive_stale")},
	{"pg_cron_job_failed", k("pg_cron_job_failed")},
	{"pg_cron_history_unbounded", k("pg_cron_history_unbounded")},
	{"partman_premake_gap", k("partman_premake_gap")},
	{"partman_default_rows", k("partman_default_rows")},
	{"timescale_job_failed", k("timescale_job_failed")},
	{"timescale_cagg_stale", k("timescale_cagg_stale")},
}

// AnalyzerRules returns a copy of the rule-pack vocabulary.
func AnalyzerRules() []Rule {
	out := make([]Rule, len(analyzerRules))
	for i, r := range analyzerRules {
		out[i] = Rule{Name: r.Name, Categories: append([]string(nil), r.Categories...)}
	}
	return out
}

// protectedCategories can never be disabled: each guards against an
// outage, and the sage_* self rules keep pg_sage's own cost visible.
var protectedCategories = map[string]string{
	"wraparound_freeze": "transaction ID wraparound stops every write to the database",
	"xid_wraparound":    "transaction ID wraparound stops every write to the database",
	"sequence_exhaustion": "an exhausted sequence makes every insert that " +
		"uses it fail",
}

const selfRuleReason = "the sage_* rules keep pg_sage's own cost on the " +
	"database visible"

// ProtectedReason reports why a category cannot be disabled.
func ProtectedReason(category string) (string, bool) {
	if reason, ok := protectedCategories[category]; ok {
		return reason, true
	}
	if strings.HasPrefix(category, "sage_") {
		return selfRuleReason, true
	}
	return "", false
}

// Disabled is a resolved rule pack: the snapshot rules to skip and the
// categories whose findings are dropped and resolved as rule_disabled.
type Disabled struct {
	Rules      map[string]bool
	Categories map[string]bool
}

// Any reports whether anything is disabled.
func (d Disabled) Any() bool { return len(d.Rules) > 0 || len(d.Categories) > 0 }

// ResolveDisabled expands rule and category names. A name is an analyzer
// rule (unused_indexes) or an analyzer category (unused_index). An unknown,
// empty or protected name is an error; the returned set holds every name
// that resolved, so a runtime caller can keep the valid part.
func ResolveDisabled(names []string) (Disabled, error) {
	d := Disabled{Rules: map[string]bool{}, Categories: map[string]bool{}}
	var errs []error
	for _, name := range names {
		cats, err := disabledCategoriesOf(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if isRule(name) {
			d.Rules[name] = true
		}
		for _, c := range cats {
			d.Categories[c] = true
		}
	}
	return d, errors.Join(errs...)
}

func disabledCategoriesOf(name string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("an empty rule name")
	}
	if reason, ok := ProtectedReason(name); ok {
		return nil, fmt.Errorf("%q cannot be disabled: %s", name, reason)
	}
	cats := ruleCategories(name)
	if cats == nil {
		e, ok := Lookup(name)
		if !ok || e.Prefix || e.Name != name || e.Producer != ProducerAnalyzer {
			return nil, fmt.Errorf("%q: unknown rule or category "+
				"(see docs/findings.md for the rule names)", name)
		}
		cats = []string{name}
	}
	for _, c := range cats {
		if reason, ok := ProtectedReason(c); ok {
			return nil, fmt.Errorf("%q cannot be disabled: %s", name, reason)
		}
	}
	return cats, nil
}

func ruleCategories(name string) []string {
	for _, r := range analyzerRules {
		if r.Name == name {
			return r.Categories
		}
	}
	return nil
}

func isRule(name string) bool { return ruleCategories(name) != nil }
