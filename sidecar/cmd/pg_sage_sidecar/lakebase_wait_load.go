package main

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// maxWaitLoadEvents bounds one wait-load read of the retained counters
// (the store's own cap); the newest events are kept.
const maxWaitLoadEvents = 1000

// lakebaseWaitLoad serves api.RuntimeDeps.LakebaseWaitLoad (market-01b): it
// reads database's retained raw wait_event_counters events, whose class
// and event labels the normalized history drops, and converts them to
// average active sessions by wait class.
func lakebaseWaitLoad(ctx context.Context, database string, from, to time.Time,
) (lakebaseobs.WaitLoadSeries, error) {
	runtime, store, err := lakebaseHistorySource(database)
	if err != nil {
		return lakebaseobs.WaitLoadSeries{}, err
	}
	events, err := store.History(ctx, runtime.Binding(), lakebaseobs.WaitCounters,
		from, to, maxWaitLoadEvents, time.Now())
	if err != nil {
		return lakebaseobs.WaitLoadSeries{}, fmt.Errorf("read %s history of %s: %w",
			lakebaseobs.WaitCounters, database, err)
	}
	series, err := lakebaseobs.WaitLoad(events, from, to)
	if err != nil {
		return lakebaseobs.WaitLoadSeries{}, fmt.Errorf("wait load of %s: %w", database, err)
	}
	series.Truncated = len(events) >= maxWaitLoadEvents
	return series, nil
}
