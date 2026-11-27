package analyzer

import (
	"maps"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
)

// DetailOverriddenByTables lists, on a global setting finding, the tables
// that keep their own value of that setting (a per-table finding or an
// explicit storage parameter): the global change does not reach them.
const DetailOverriddenByTables = "overridden_by_tables"

// overrideRationale is the line a global finding gains when tables
// override it (advisors-02: show the DBA everything pg_sage knows).
const overrideRationale = "these tables keep their own setting; the global change " +
	"does not affect them"

// annotateGlobalOverrides adds, to every global ALTER SYSTEM finding of a
// GUC in overrides, the tables that override it. Lists are merged with
// any already recorded, sorted and unique. Detail maps are cloned, so a
// caller's finding is never mutated.
func annotateGlobalOverrides(findings []Finding, overrides map[string][]string) []Finding {
	if len(overrides) == 0 {
		return findings
	}
	for i, f := range findings {
		guc, perTable := extractGUCTarget(f.RecommendedSQL)
		if guc == "" || perTable || len(overrides[guc]) == 0 {
			continue
		}
		findings[i] = withOverrides(f, overrides[guc])
	}
	return findings
}

func withOverrides(f Finding, tables []string) Finding {
	prior, _ := f.Detail[DetailOverriddenByTables].([]string)
	merged := append(slices.Clone(prior), tables...)
	slices.Sort(merged)
	merged = slices.Compact(merged)
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[DetailOverriddenByTables] = merged
	if !strings.Contains(f.Recommendation, overrideRationale) {
		f.Recommendation = strings.TrimSpace(f.Recommendation + " (Tables in " +
			DetailOverriddenByTables + ": " + overrideRationale + ".)")
	}
	return f
}

// annotateReloptionOverrides records the tables of snap whose explicit
// storage parameter overrides a global finding's GUC.
func annotateReloptionOverrides(findings []Finding, snap *collector.Snapshot) []Finding {
	if snap == nil {
		return findings
	}
	overrides := map[string][]string{}
	for _, t := range snap.Tables {
		for _, opt := range t.Reloptions {
			name, _, found := strings.Cut(opt, "=")
			if !found || name == "" {
				continue
			}
			name = strings.ToLower(strings.TrimSpace(name))
			overrides[name] = append(overrides[name], t.SchemaName+"."+t.RelName)
		}
	}
	return annotateGlobalOverrides(findings, overrides)
}
