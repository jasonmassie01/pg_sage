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
		cat != "query_tuning"
}

// DedupFindings resolves conflicts among findings targeting the
// same object.
//
// Rules:
//  1. Same ObjectIdentifier + same Category: keep highest
//     severity (re-detection, not a conflict).
//  2. Same ObjectIdentifier + different Categories: keep all
//     (different aspects of same object).
//  3. query_tuning beats global config advisor categories
//     for the same object (per-query > global).
//  4. If same severity, keep the one with RecommendedSQL.
func DedupFindings(
	findings []Finding,
	logFn func(string, string, ...any),
) []Finding {
	if len(findings) == 0 {
		return findings
	}

	groups := groupByObject(findings)
	result := make([]Finding, 0, len(findings))
	for _, group := range groups {
		resolved := resolveGroup(group, logFn)
		result = append(result, resolved...)
	}
	return result
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
		if f.Category == "query_tuning" {
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
				"dedup: query_tuning beats %s for %s",
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
