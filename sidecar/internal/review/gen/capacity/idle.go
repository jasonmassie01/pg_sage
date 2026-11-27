package capacity

import (
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// IdleDays is the fewest quiet days that make a database idle.
const IdleDays = 14

// DayCalls is one UTC day of a database's statement history. Observed is
// true when pg_sage's rollup saw the day (any rollup row, pg_sage's own
// included, or a coverage hour); WorkloadCalls are the day's calls of
// every statement but pg_sage's own (a statement whose text is unknown
// counts as workload, so it can only keep a database active).
type DayCalls struct {
	Day           time.Time
	Observed      bool
	WorkloadCalls int64
}

// QuietDays is how many of the newest days, in a row, were observed with
// no workload call. A day pg_sage did not observe ends the run: no
// history is no proof of quiet. The days need not be in order.
func QuietDays(days []DayCalls) int {
	sorted := slices.Clone(days)
	slices.SortFunc(sorted, func(a, b DayCalls) int { return a.Day.Compare(b.Day) })
	n := 0
	for i := len(sorted) - 1; i >= 0; i-- {
		if !sorted[i].Observed || sorted[i].WorkloadCalls > 0 {
			break
		}
		n++
	}
	return n
}

// Idle is the idle-database rule (fleetpol-03 reuses it): IdleDays or
// more quiet days.
func Idle(days []DayCalls) bool { return QuietDays(days) >= IdleDays }

// idle emits the idle-database advice, with the database's size and,
// with a storage rate, its monthly storage cost.
func (b *builder) idle(db workpack.Series) {
	if b.in.Days == nil {
		b.cov.AddMissing(MissingActivity)
		b.cov.Note("idle: the workload calls were not read, so idleness is not judged")
		return
	}
	quiet := QuietDays(b.in.Days)
	if quiet < IdleDays {
		b.cov.Note("idle: %d days without workload calls; a database is idle after %d",
			quiet, IdleDays)
		return
	}
	q := map[string]float64{QuantityQuietDays: float64(quiet)}
	size, known := lastSize(db)
	sizeText := "its size is not measured"
	if known {
		q[QuantitySizeBytes] = size
		sizeText = "it holds " + bytesText(size)
	}
	money := storageCost(b.in.Rates, size, known, q)
	b.emit(candidate.Candidate{Kind: KindIdleDatabase, Target: b.databaseTarget(),
		EvidenceIDs: []string{db.ID, coverageEvidence},
		Reason: fmt.Sprintf("No workload calls (pg_sage's own statements excluded) for "+
			"the last %d days; %s. Monthly storage cost: %s. If nothing needs it, archive "+
			"and drop it: that is an operator's decision, never pg_sage's.", quiet,
			sizeText, money),
		Prediction: candidate.Prediction{Metric: "idle_databases", Direction: "decrease",
			Source: "query rollups by day and the daily database size", Quantities: q}})
}

// lastSize is the newest measured value of s.
func lastSize(s workpack.Series) (float64, bool) {
	for i := len(s.Values) - 1; i >= 0; i-- {
		if v := s.Values[i]; v != nil {
			return *v, true
		}
	}
	return 0, false
}

// storageCost states size's monthly storage cost at the operator's rate
// (recording it in q), or why it is unknown.
func storageCost(r *cost.Rates, size float64, known bool, q map[string]float64) string {
	switch {
	case r == nil:
		return NoRates
	case r.StorageGBMonth <= 0:
		return "unknown: no storage rate in cost.rates"
	case !known:
		return "unknown: the database size is not measured"
	}
	v := size / float64(1<<30) * r.StorageGBMonth
	q[QuantityMonthlyCost] = v
	return fmt.Sprintf("%.2f %s per month at the operator's storage rate (%s, as of %s)",
		v, r.Currency, r.Source, r.AsOf.Format(time.DateOnly))
}
