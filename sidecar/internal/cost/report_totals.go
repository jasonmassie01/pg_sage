package cost

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// add prices one instance and attributes its compute cost.
func (s *Section) add(g *group, window time.Duration) {
	vcpus := g.rates.VCPUs
	if vcpus == 0 && g.latest != nil {
		vcpus = g.latest.VCPUs
	}
	a := Attribute(g.rates.ComputePerHour, window, vcpus, g.sortedDeltas())
	rc := ResourceCost{Resource: g.key, Currency: g.rates.Currency,
		RateSource: g.rates.Source, AsOf: g.rates.AsOf.Format(time.DateOnly), VCPUs: vcpus,
		Compute: a.ComputeCost, ExecSeconds: a.TotalExecSeconds, IdleShare: a.IdleShare}
	for _, m := range g.members {
		rc.Databases = append(rc.Databases, m.Database)
	}
	months := window.Hours() / hoursPerMonth
	if g.latest != nil && g.latest.AllocatedStorageBytes > 0 && g.rates.StorageGBMonth > 0 {
		v := g.rates.StorageGBMonth * g.latest.AllocatedStorageBytes / gib * months
		rc.Storage = &v
	}
	if g.rates.IOPSMonth > 0 && g.rates.ProvisionedIOPS > 0 {
		v := g.rates.IOPSMonth * float64(g.rates.ProvisionedIOPS) * months
		rc.IOPS = &v
	}
	rc.Total = rc.Compute + deref(rc.Storage) + deref(rc.IOPS)
	for _, q := range a.Queries {
		q.Currency = rc.Currency
		s.TopQueries = append(s.TopQueries, q)
	}
	s.DroppedPairs += g.dropped + a.Dropped
	if len(g.errs) > 0 {
		s.Partial = true
		s.Unavailable = append(s.Unavailable, g.errs...)
	}
	s.Resources = append(s.Resources, rc)
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// finish sorts the top list and totals the instances when they share a
// currency (mixed currencies have per-resource figures only).
func (s *Section) finish() {
	s.Available = len(s.Resources) > 0
	sort.SliceStable(s.TopQueries, func(i, j int) bool {
		return s.TopQueries[i].Cost > s.TopQueries[j].Cost
	})
	if len(s.TopQueries) > TopQueries {
		s.TopQueries = s.TopQueries[:TopQueries]
	}
	s.RateSource, s.AsOf, s.Currency = SourceConfig, s.Resources[0].AsOf,
		s.Resources[0].Currency
	var compute, total, exec, vcpuSeconds float64
	var storage, iops *float64
	for _, rc := range s.Resources {
		if rc.AsOf < s.AsOf {
			s.AsOf = rc.AsOf
		}
		if rc.Currency != s.Currency {
			s.Currency = "mixed"
		}
		compute, total = compute+rc.Compute, total+rc.Total
		storage, iops = addOpt(storage, rc.Storage), addOpt(iops, rc.IOPS)
		if rc.VCPUs > 0 {
			exec += rc.ExecSeconds
			vcpuSeconds += s.WindowHours * 3600 * float64(rc.VCPUs)
		}
	}
	if vcpuSeconds > 0 {
		idle := max(0, 1-exec/vcpuSeconds)
		s.IdleShare = &idle
	}
	if s.Currency == "mixed" {
		return
	}
	s.Compute, s.Total, s.Storage, s.IOPS = &compute, &total, storage, iops
}

func addOpt(sum, v *float64) *float64 {
	if v == nil {
		return sum
	}
	out := *v + deref(sum)
	return &out
}

// PGReader reads the history in each member's own database.
type PGReader struct{}

// Deltas implements Reader.
func (PGReader) Deltas(ctx context.Context, m Member, from, to time.Time) ([]QueryDelta,
	int, error) {
	if m.Pool == nil {
		return nil, 0, errors.New("database has no connection")
	}
	return QueryDeltas(ctx, m.Pool, m.Database, from, to)
}

const latestHourSQL = `/* pg_sage */ SELECT vcpus, COALESCE(allocated_storage_bytes, 0)
FROM sage.host_usage_hourly WHERE resource = $1 ORDER BY hour DESC LIMIT 1`

// LatestHour implements Reader: the instance's newest hourly row (nil
// when none is recorded).
func (PGReader) LatestHour(ctx context.Context, m Member) (*HourRow, error) {
	if m.Pool == nil {
		return nil, errors.New("database has no connection")
	}
	var h HourRow
	err := m.Pool.QueryRow(ctx, latestHourSQL, m.Resource).Scan(&h.VCPUs,
		&h.AllocatedStorageBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}
