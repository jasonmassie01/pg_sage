package api

import (
	"fmt"
	"math"
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

// timeToImpact is the case's assessed time to impact; false when it has
// none usable (unassessed, missing, non-finite or not positive).
func timeToImpact(c cases.Case) (float64, bool) {
	u := c.Urgency
	if !u.Assessed || u.TimeToImpactSeconds == nil {
		return 0, false
	}
	secs := *u.TimeToImpactSeconds
	return secs, secs > 0 && !math.IsNaN(secs) && !math.IsInf(secs, 0)
}

// sortCasesGlobally ranks cases from every source and database by
// severity; within a severity an assessed time to impact ranks first,
// nearest first (cases-07), then most recent observation, then database
// and id so the order is reproducible (SURF-11 / G9-B16). Each case
// carries the basis of its rank.
func sortCasesGlobally(list []cases.Case) {
	for i := range list {
		list[i].RankBasis = rankBasis(list[i])
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		ta, oka := timeToImpact(a)
		tb, okb := timeToImpact(b)
		if oka != okb {
			return oka
		}
		if oka && ta != tb {
			return ta < tb
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

// rankBasis says why a case ranks where it does.
func rankBasis(c cases.Case) string {
	if secs, ok := timeToImpact(c); ok {
		return fmt.Sprintf("severity %s; time to impact about %s", c.Severity,
			cases.HumanDuration(secs))
	}
	return fmt.Sprintf("severity %s; urgency not assessed; most recent first", c.Severity)
}
