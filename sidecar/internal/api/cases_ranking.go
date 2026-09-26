package api

import (
	"sort"

	"github.com/pg-sage/sidecar/internal/cases"
)

func severityRank(s cases.Severity) int {
	switch s {
	case cases.SeverityCritical:
		return 3
	case cases.SeverityWarning:
		return 2
	case cases.SeverityInfo:
		return 1
	default:
		return 0
	}
}

// sortCasesGlobally ranks cases from every source and database by
// severity, then most recent observation, then database and id so the
// order is reproducible (SURF-11 / G9-B16).
func sortCasesGlobally(list []cases.Case) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.After(b.ObservedAt)
		}
		if a.DatabaseName != b.DatabaseName {
			return a.DatabaseName < b.DatabaseName
		}
		return a.ID < b.ID
	})
}
