package fleet

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/review/gen/capacity"
	"github.com/pg-sage/sidecar/internal/store"
)

// censusLookback is how far back a full member's rollups are read.
const censusLookback = 60 * 24 * time.Hour

// censusReadTimeout bounds each statement the census runs on a member.
const censusReadTimeout = "5s"

// Activity bases, as a census row names them.
const (
	basisRollups = "query rollups by day (evid-01), pg_sage's own statements excluded " +
		"(review-02f's idle rule)"
	basisStatements = "pg_stat_statements calls by roles other than pg_sage's, read once " +
		"per observe-only round"
)

// CensusActivityReader reads an observe-only member's newest reading
// (*CensusDB).
type CensusActivityReader interface {
	CensusActivity(ctx context.Context, key string) (CensusActivity, error)
}

// NewCensusMeasure measures a full member from its runtime's history
// (review-02f's idle rule over the evid-01 rollups) and its size, and an
// observe-only member from the sweeper's readings. It only reads.
func NewCensusMeasure(instances func(name string) *DatabaseInstance,
	act CensusActivityReader, now func() time.Time) CensusMeasureFunc {
	return func(ctx context.Context, s CensusSubject) (CensusMeasure, error) {
		switch s.Kind {
		case CensusFull:
			var inst *DatabaseInstance
			if instances != nil {
				inst = instances(s.Key)
			}
			return measureFull(ctx, s.Key, inst, now())
		case CensusObserveOnly:
			return measureObserveOnly(ctx, s.Key, act)
		}
		return CensusMeasure{}, fmt.Errorf("census: %s: no measure for kind %q", s.Key,
			s.Kind)
	}
}

func measureFull(ctx context.Context, key string, inst *DatabaseInstance,
	now time.Time) (CensusMeasure, error) {
	if inst == nil || inst.Pool == nil {
		return CensusMeasure{}, fmt.Errorf("%w: %s has no runtime connection",
			ErrCensusUnreachable, key)
	}
	size, err := databaseSize(ctx, inst.Pool)
	if err != nil {
		return CensusMeasure{}, fmt.Errorf("census: %s: %w", key, err)
	}
	days, err := capacity.NewDBCallSource(inst.Pool).Read(ctx, now.Add(-censusLookback), now)
	if err != nil {
		return CensusMeasure{SizeBytes: size, Basis: basisRollups,
			Unmeasured: "the workload calls are unreadable: " + err.Error()}, nil
	}
	m := censusFromDays(days)
	m.SizeBytes = size
	return m, nil
}

// databaseSize reads the member's size in a bounded read-only transaction;
// a failed connection is ErrCensusUnreachable.
func databaseSize(ctx context.Context, pool *pgxpool.Pool) (*int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCensusUnreachable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, stmt := range []string{"SET TRANSACTION READ ONLY",
		"SET LOCAL statement_timeout = '" + censusReadTimeout + "'"} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	var size int64
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT
		pg_catalog.pg_database_size(pg_catalog.current_database())`).Scan(&size); err != nil {
		return nil, fmt.Errorf("reading pg_database_size: %w", err)
	}
	return &size, nil
}

// censusFromDays applies review-02f's idle rule to a member's days: the
// quiet run is capacity.QuietDays, the observation is the unbroken run of
// observed days ending at the newest one (through that day's end), and the
// last activity is the newest observed day with workload calls (day
// resolution).
func censusFromDays(days []capacity.DayCalls) CensusMeasure {
	sorted := slices.Clone(days)
	slices.SortFunc(sorted, func(a, b capacity.DayCalls) int { return a.Day.Compare(b.Day) })
	newest := -1
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i].Observed {
			newest = i
			break
		}
	}
	if newest < 0 {
		return CensusMeasure{Basis: basisRollups,
			Unmeasured: "no query rollup history in the last 60 days"}
	}
	from := newest
	for from > 0 && sorted[from-1].Observed {
		from--
	}
	to := sorted[newest].Day.Add(24 * time.Hour)
	m := CensusMeasure{ObservedFrom: &sorted[from].Day, ObservedTo: &to, Basis: basisRollups,
		QuietFor: time.Duration(capacity.QuietDays(sorted)) * 24 * time.Hour}
	for i := newest; i >= 0; i-- {
		if sorted[i].Observed && sorted[i].WorkloadCalls > 0 {
			m.LastActivityAt = &sorted[i].Day
			break
		}
	}
	return m
}

func measureObserveOnly(ctx context.Context, key string,
	act CensusActivityReader) (CensusMeasure, error) {
	if act == nil {
		return CensusMeasure{Unmeasured: "no activity reader is configured"}, nil
	}
	a, err := act.CensusActivity(ctx, key)
	if err != nil {
		return CensusMeasure{}, err
	}
	if a.SweepStatus == store.ObserveUnreachable {
		return CensusMeasure{}, fmt.Errorf("%w: %s: the newest observe-only sweep could "+
			"not connect: %s", ErrCensusUnreachable, key, a.SweepError)
	}
	return censusFromReading(a), nil
}

// censusFromReading is the same rule over the sweeper's readings: quiet
// since the newer of the baseline and the last rise of the calls.
func censusFromReading(a CensusActivity) CensusMeasure {
	switch {
	case !a.Found:
		return CensusMeasure{Basis: basisStatements, Unmeasured: "not swept with an " +
			"activity reading yet: the observe-only sweep reads pg_stat_statements once " +
			"per round"}
	case a.WorkloadCalls == nil:
		return CensusMeasure{Basis: basisStatements, SizeBytes: a.SizeBytes,
			Unmeasured: "pg_stat_statements is not readable in the database (not " +
				"installed, or not visible to pg_sage's role): real activity is unknown"}
	}
	quietFrom := a.BaselineAt
	if a.LastActivityAt != nil && a.LastActivityAt.After(quietFrom) {
		quietFrom = *a.LastActivityAt
	}
	base, read := a.BaselineAt, a.ReadAt
	return CensusMeasure{LastActivityAt: a.LastActivityAt, ObservedFrom: &base,
		ObservedTo: &read, QuietFor: max(a.ReadAt.Sub(quietFrom), 0),
		SizeBytes: a.SizeBytes, Basis: basisStatements}
}
