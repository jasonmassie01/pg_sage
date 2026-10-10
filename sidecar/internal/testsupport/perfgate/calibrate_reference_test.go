package perfgate

import (
	"regexp"
	"slices"
	"testing"
)

var referenceDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// The reference runner's workload times are the medians of the recorded
// reference runs: at least 10 dispatched large-scale runs, each complete
// and recorded once. Both are pinned, so moving the reference is a
// reviewed change to this test as well as to the constants.
func TestReferenceTimesAreTheMediansOfTheRecordedRuns(t *testing.T) {
	if ReferenceCPUMs != 63.0 || ReferenceDBMs != 129.75 {
		t.Fatalf("reference CPU %v ms, SQL %v ms; pinned at 63.0 and 129.75",
			ReferenceCPUMs, ReferenceDBMs)
	}
	if len(referenceRuns) < 10 {
		t.Fatalf("%d reference runs recorded, want at least 10", len(referenceRuns))
	}
	seen := map[int64]bool{}
	cpu := make([]float64, 0, len(referenceRuns))
	db := make([]float64, 0, len(referenceRuns))
	for _, r := range referenceRuns {
		if r.id <= 0 || seen[r.id] || !referenceDate.MatchString(r.date) ||
			r.cpuModel == "" || r.cpus < 1 || !(r.cpuMs > 0) || !(r.dbMs > 0) {
			t.Fatalf("reference run incomplete or recorded twice: %+v", r)
		}
		seen[r.id] = true
		cpu, db = append(cpu, r.cpuMs), append(db, r.dbMs)
	}
	if !near(ReferenceCPUMs, medianOf(cpu)) || !near(ReferenceDBMs, medianOf(db)) {
		t.Fatalf("reference CPU %v ms, SQL %v ms; the recorded runs' medians are %v and %v",
			ReferenceCPUMs, ReferenceDBMs, medianOf(cpu), medianOf(db))
	}
}

func TestMedianOf(t *testing.T) {
	if got := medianOf([]float64{3, 1, 2}); got != 2 {
		t.Fatalf("median of 3 values = %v, want 2", got)
	}
	if got := medianOf([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("median of 4 values = %v, want 2.5", got)
	}
}

func medianOf(xs []float64) float64 {
	s := slices.Sorted(slices.Values(xs))
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
