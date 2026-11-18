package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/store"
)

// reviewAndEnrollFixtures renders, from fixture numbers, the T6 families
// whose state is process-wide and empty until a grade, a trigger run or a
// discovery cycle happens (review-01a, review-04a, fleetpol-01b), each only
// when scrape lacks it, so a family a real counter already rendered is
// never written twice. The enrollment families are declared for meta mode
// only.
func reviewAndEnrollFixtures(scrape, mode string) string {
	var b strings.Builder
	has := func(name string) bool { return strings.Contains(scrape, "# TYPE "+name+" ") }
	if !has(reviewMetric) {
		writeFirstGradeMetrics(&b, &firstGrades{secs: map[string]float64{"alpha": 42}})
	}
	var errs map[trigger.Kind]int64
	var runs map[string]int64
	if !has(reviewTriggerErrorsMetric) {
		errs = map[trigger.Kind]int64{"pre_upgrade": 1}
	}
	if !has(reviewRunsMetric) {
		runs = map[string]int64{"calendar": 2, "event": 1}
	}
	writeReviewTriggerCounts(&b, errs, runs)
	if mode == metricsreg.ModeMeta && !has("pg_sage_fleet_enroll_seconds") {
		discovery.WriteEnrollMetrics(&b, []store.EnrollStat{{Set: "prod", Enrolled: 3,
			P50Seconds: 60, P95Seconds: 300, Eligible: 2, AtBaseline: 1}})
	}
	return b.String()
}
