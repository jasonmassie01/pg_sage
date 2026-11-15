package verify

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// minBucket is the finest bucket. Empty buckets do not count, so a bucket
// finer than the collector's cadence holds one sample interval.
const minBucket = 5 * time.Second

// BucketWidth sizes the time buckets of a window: about 48 buckets, never
// finer than minBucket or coarser than 6 hours.
func BucketWidth(window time.Duration) time.Duration {
	width := window / 48
	switch {
	case width < minBucket:
		return minBucket
	case width > 6*time.Hour:
		return 6 * time.Hour
	}
	return width
}

// queryMeasurement summarizes one query's window from its bucketed
// intervals (querystore.QueryIntervals, which owns the anchor and epoch
// rules): calls, call-weighted mean and its standard error.
func (s *PostgresObservationSource) queryMeasurement(
	ctx context.Context, id int64, from, to time.Time,
) (Measurement, error) {
	buckets, err := querystore.QueryIntervals(ctx, s.historyStore(), id, from, to,
		BucketWidth(to.Sub(from)))
	if err != nil {
		return Measurement{}, err
	}
	intervals := make([]Interval, 0, len(buckets))
	for _, b := range buckets {
		if b.EpochBreak {
			// The window spans more than one statistics epoch (R10): no
			// measurement, never a partial one.
			return Measurement{}, nil
		}
		intervals = append(intervals, Interval{Calls: b.Calls, TotalMs: b.TotalMs})
	}
	return Summarize(intervals), nil
}
