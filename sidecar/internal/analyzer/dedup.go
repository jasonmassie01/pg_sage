package analyzer

import "strings"

// severityRank returns a numeric rank for severity comparison.
// Higher rank = more severe.
func severityRank(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// isGlobalConfigCategory returns true for advisor categories
// that represent global configuration recommendations.
func isGlobalConfigCategory(cat string) bool {
	return strings.HasSuffix(cat, "_tuning") &&
		!isPerQueryCategory(cat)
}

// isPerQueryCategory returns true for categories that represent
// per-query tuning findings (tuner, hint, query_tuning).
func isPerQueryCategory(cat string) bool {
	if cat == "query_tuning" {
		return true
	}
	return strings.Contains(cat, "hint") ||
		strings.Contains(cat, "tuner")
}

// isVacuumCategory returns true for vacuum-related findings.
func isVacuumCategory(cat string) bool {
	return strings.Contains(cat, "vacuum")
}

// DedupFindings resolves conflicts among findings targeting the
// same object. Delegates to DeduplicateFindings with no I/O
// utilization data.
func DedupFindings(
	findings []Finding,
	logFn func(string, string, ...any),
) []Finding {
	return DeduplicateFindings(findings, 0, logFn)
}

// DeduplicateFindings resolves conflicts among findings targeting
// the same object.
//
// Rules:
//  1. Same ObjectIdentifier + same Category: keep highest
//     severity (re-detection, not a conflict).
//  2. Same ObjectIdentifier + different Categories: keep all
//     (different aspects of same object).
//  3. Per-query categories (query_tuning, *hint*, *tuner*)
//     beat global config advisor categories for the same object.
//  4. If same severity, keep the one with RecommendedSQL.
//  5. If ioUtilPct > 50, downgrade vacuum findings to "info".
func DeduplicateFindings(
	findings []Finding,
	ioUtilPct float64,
	logFn func(string, string, ...any),
) []Finding {
	if len(findings) == 0 {
		return findings
	}

	if ioUtilPct > 50 {
		findings = downgradeVacuumFindings(findings, logFn)
	}

	groups := groupByObject(findings)
	result := make([]Finding, 0, len(findings))
	for _, group := range groups {
		resolved := resolveGroup(group, logFn)
		result = append(result, resolved...)
	}
	return result
}

// downgradeVacuumFindings sets vacuum findings to "info" when
// the system is I/O bound (>50% of query time spent on I/O).
func downgradeVacuumFindings(
	findings []Finding,
	logFn func(string, string, ...any),
) []Finding {
	out := make([]Finding, len(findings))
	copy(out, findings)
	for i := range out {
		if isVacuumCategory(out[i].Category) &&
			out[i].Severity != "info" {
			logFn(
				"DEBUG",
				"dedup: I/O >50%%, downgrading %s to info",
				out[i].Title,
			)
			out[i].Severity = "info"
		}
	}
	return out
}

// groupByObject buckets findings by ObjectIdentifier,
// preserving insertion order of first-seen keys.
func groupByObject(
	findings []Finding,
) [][]Finding {
	idx := make(map[string]int)
	var groups [][]Finding
	for _, f := range findings {
		i, ok := idx[f.ObjectIdentifier]
		if !ok {
			i = len(groups)
			idx[f.ObjectIdentifier] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], f)
	}
	return groups
}

// resolveGroup deduplicates findings within a single object.
func resolveGroup(
	group []Finding,
	logFn func(string, string, ...any),
) []Finding {
	if len(group) <= 1 {
		return group
	}

	// Apply query_tuning-vs-global rule first.
	group = applyQueryTuningRule(group, logFn)

	// Deduplicate same-category pairs.
	return dedupSameCategory(group, logFn)
}

// applyQueryTuningRule removes global config findings when a
// query_tuning finding exists for the same object.
func applyQueryTuningRule(
	group []Finding,
	logFn func(string, string, ...any),
) []Finding {
	hasQueryTuning := false
	for _, f := range group {
		if isPerQueryCategory(f.Category) {
			hasQueryTuning = true
			break
		}
	}
	if !hasQueryTuning {
		return group
	}

	kept := make([]Finding, 0, len(group))
	for _, f := range group {
		if isGlobalConfigCategory(f.Category) {
			logFn(
				"DEBUG",
				"dedup: per-query tuning beats %s for %s",
				f.Category, f.ObjectIdentifier,
			)
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// dedupSameCategory keeps only the best finding per category
// within a group sharing the same ObjectIdentifier.
func dedupSameCategory(
	group []Finding,
	logFn func(string, string, ...any),
) []Finding {
	best := make(map[string]int) // category -> index in result
	result := make([]Finding, 0, len(group))

	for _, f := range group {
		prev, exists := best[f.Category]
		if !exists {
			best[f.Category] = len(result)
			result = append(result, f)
			continue
		}
		winner := pickBetter(result[prev], f)
		if winner.Title != result[prev].Title {
			logFn(
				"DEBUG",
				"dedup: replacing %q with %q for %s",
				result[prev].Title, winner.Title,
				f.ObjectIdentifier,
			)
		}
		result[prev] = winner
	}
	return result
}

// pickBetter returns the preferred finding when two share the
// same category and object.
func pickBetter(a, b Finding) Finding {
	ra, rb := severityRank(a.Severity), severityRank(b.Severity)
	if ra != rb {
		if ra > rb {
			return a
		}
		return b
	}
	// Same severity: prefer the one with actionable SQL.
	if a.RecommendedSQL != "" && b.RecommendedSQL == "" {
		return a
	}
	if b.RecommendedSQL != "" && a.RecommendedSQL == "" {
		return b
	}
	// All else equal, keep first.
	return a
}
