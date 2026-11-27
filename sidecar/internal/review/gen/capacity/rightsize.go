package capacity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// StorageHeadroomShare is the share of the allocated storage the 90-day
// projection (plus its band) may reach before the review advises more.
const StorageHeadroomShare = 0.85

// Rightsizing is the cost package's verdict on the database's instance
// and the instance's allocated storage (0: not reported).
type Rightsizing struct {
	Resource              string
	Verdict               cost.Verdict
	AllocatedStorageBytes float64
}

// Rightsizer judges the database's instance.
type Rightsizer interface {
	Rightsize(ctx context.Context) (Rightsizing, error)
}

// CostRightsizer judges the instance with cost.EvaluateRightsize (which
// records its advisory finding, as the hourly loop does) over pool's
// hourly usage, priced by Rates when cost.rates has an entry. Now is the
// clock (nil: time.Now).
type CostRightsizer struct {
	Pool     *pgxpool.Pool
	Resource string
	Database string
	Rates    cost.RateSource
	Now      func() time.Time
}

// Rightsize implements Rightsizer.
func (r CostRightsizer) Rightsize(ctx context.Context) (Rightsizing, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	v, err := cost.EvaluateRightsize(ctx, r.Pool, r.Resource, r.Database, r.Rates, now)
	if err != nil {
		return Rightsizing{}, err
	}
	rows, err := cost.LoadHours(ctx, r.Pool, r.Resource, now.Add(-cost.UsageRetention))
	if err != nil {
		return Rightsizing{}, fmt.Errorf("read the allocated storage of %s: %w",
			r.Resource, err)
	}
	out := Rightsizing{Resource: r.Resource, Verdict: v}
	var newest time.Time
	for _, h := range rows {
		if h.Hour.After(newest) {
			newest, out.AllocatedStorageBytes = h.Hour, h.AllocatedStorageBytes
		}
	}
	return out, nil
}

// rightsize turns the verdict into advice, or says why there is none.
func (b *builder) rightsize() {
	switch {
	case b.in.RightsizeErr != nil:
		b.cov.AddMissing(MissingRightsize)
		b.cov.Note("right-size: unknown: %v", b.in.RightsizeErr)
	case b.in.Rightsizing == nil:
		b.cov.Note("right-size: unknown: no cloud provider resource (SQL alone gives " +
			"growth and idle databases)")
	default:
		rs := *b.in.Rightsizing
		v := rs.Verdict
		switch v.Decision {
		case cost.DecisionDownsize, cost.DecisionUpsize:
			b.emitRightsize(rs)
		case cost.DecisionKeep:
			b.cov.Note("right-size %s: keep %s: %s", rs.Resource, v.CurrentClass, v.Reason)
		default:
			b.cov.AddMissing(MissingRightsize)
			b.cov.Note("right-size %s: %s", rs.Resource, unknownText(v.Reason))
		}
	}
}

// unknownText is reason stated as unknown once.
func unknownText(reason string) string {
	if strings.HasPrefix(reason, "unknown") {
		return reason
	}
	if reason == "" {
		reason = "no verdict"
	}
	return "unknown: " + reason
}

func (b *builder) instanceTarget(resource string) string {
	if resource == "" {
		return "instance:" + b.env.Database
	}
	return "instance:" + resource
}

func (b *builder) emitRightsize(rs Rightsizing) {
	v := rs.Verdict
	q := map[string]float64{QuantityCurrentVCPUs: float64(v.CurrentVCPUs),
		QuantityTargetVCPUs: float64(v.TargetVCPUs), QuantityCPUP95: v.CPUP95,
		QuantityProjectedP95: v.ProjectedCPUP95}
	money := NoRates
	if v.MonthlyDelta != nil {
		q[QuantityMonthlyDelta] = *v.MonthlyDelta
		money = fmt.Sprintf("%+.2f %s at the operator's rates (%s, as of %s)",
			*v.MonthlyDelta, v.Currency, v.RateSource, v.AsOf.Format(time.DateOnly))
	}
	dir := "decrease"
	if v.Decision == cost.DecisionUpsize {
		dir = "increase"
	}
	reason := fmt.Sprintf("%s %s (%d vCPUs) to %s (%d vCPUs): %s. CPU p95 %.1f%% "+
		"projects to %.1f%% (max %.1f%%). Monthly compute change: %s. An instance change "+
		"is an operator step at the provider; pg_sage never makes it at any trust level.",
		v.Decision, v.CurrentClass, v.CurrentVCPUs, v.TargetClass, v.TargetVCPUs, v.Reason,
		v.CPUP95, v.ProjectedCPUP95, v.ProjectedCPUMax, money)
	b.emit(candidate.Candidate{Kind: KindRightSize, Target: b.instanceTarget(rs.Resource),
		EvidenceIDs: b.instanceEvidence(), Reason: reason,
		Prediction: candidate.Prediction{Metric: "vcpus", Direction: dir,
			Source: "cost right-sizing of the hourly host usage", Quantities: q}})
}

// instanceEvidence is what an instance candidate cites in the pack.
func (b *builder) instanceEvidence() []string {
	if b.db != nil {
		return []string{b.db.Series, coverageEvidence}
	}
	return []string{coverageEvidence}
}

// storage advises more storage when the database's 90-day projection,
// with its band, reaches StorageHeadroomShare of the allocation. The
// database is only part of what the instance stores (WAL, other
// databases), so this is a lower bound.
func (b *builder) storage() {
	rs := b.in.Rightsizing
	if rs == nil || rs.AllocatedStorageBytes <= 0 || b.db == nil {
		return
	}
	a, ok := b.db.At(90)
	if !ok {
		return
	}
	alloc := rs.AllocatedStorageBytes
	share := (a.Bytes + a.Band) / alloc
	if share < StorageHeadroomShare {
		return
	}
	b.emit(candidate.Candidate{Kind: KindStorageHeadroom,
		Target: b.instanceTarget(rs.Resource), EvidenceIDs: b.instanceEvidence(),
		Reason: fmt.Sprintf("The database alone is projected at %s ± %s in 90 days, "+
			"%.0f%% of the %s allocated (other databases and WAL come on top). Grow the "+
			"instance's storage before then: an operator step at the provider, never "+
			"pg_sage's.", bytesText(a.Bytes), bytesText(a.Band), 100*share,
			bytesText(alloc)),
		Prediction: candidate.Prediction{Metric: "storage_headroom", Direction: "increase",
			Source: "the 90-day growth projection and the instance's allocated storage",
			Quantities: map[string]float64{QuantityAllocatedBytes: alloc,
				QuantityProjected90: a.Bytes, QuantityBand90: a.Band,
				QuantityAllocatedPct: 100 * share}}})
}
