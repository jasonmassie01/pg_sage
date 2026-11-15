package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Under history.store: meta the query_store samples live in the meta
// database, whose DEFAULT now() stamps their captured_at, while
// action_log.executed_at and the monitor's clock are the monitored
// database's. The monitor's sample windows are moved onto the meta
// database's clock by the difference of the two clocks, so a meta database
// whose clock leads the monitored one's does not drop the newest samples
// (executor-31).

// historyShift is how far the clock that stamps pool's query_store samples
// leads pool's own clock: 0, without a read, when the history is the
// monitored database's own.
func historyShift(ctx context.Context, pool *pgxpool.Pool) (time.Duration, error) {
	store := histstore.Resolve(pool)
	if !store.Scoped() {
		return 0, nil
	}
	own, err := poolClock(pool)(ctx)
	if err != nil {
		return 0, err
	}
	readCtx, cancel := context.WithTimeout(ctx, monitorClockTimeout)
	defer cancel()
	var meta time.Time
	if err := store.QueryRow(readCtx, monitorClockSQL).Scan(&meta); err != nil {
		return 0, fmt.Errorf("read the meta database clock (history.store: meta): %w", err)
	}
	return meta.Sub(own), nil
}

// shiftedObserver reads observe over a window on the monitored database's
// clock moved by shift, the lead of the clock that stamped the samples. A
// failed shift is a failed read: the samples are not read on a clock they
// were not stamped by.
func shiftedObserver(observe queryObserver,
	shift func(context.Context) (time.Duration, error)) queryObserver {
	return func(ctx context.Context, ids []int64, from, to time.Time) (
		map[int64]verify.Measurement, error) {
		d, err := shift(ctx)
		if err != nil {
			return nil, fmt.Errorf("align the sample window with the history clock: %w", err)
		}
		return observe(ctx, ids, from.Add(d), to.Add(d))
	}
}

// historyObserver reads pool's query_store samples over windows on pool's
// clock, wherever the history lives.
func historyObserver(pool *pgxpool.Pool) queryObserver {
	return shiftedObserver(verify.NewPostgresObservationSource(pool).QueryMeasurements,
		func(ctx context.Context) (time.Duration, error) { return historyShift(ctx, pool) })
}
