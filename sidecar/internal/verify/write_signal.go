package verify

import (
	"context"
	"fmt"
)

// The write signal reads pg_stat_database.blk_write_time from the system
// snapshots: database-wide, cumulative and non-zero only with
// track_io_timing. It cannot charge any write cost to one index (a night
// build followed by daytime load moves it whatever the index costs), so it
// is advisory evidence, never a revert reason, until a table-scoped signal
// replaces it (executor-03b).
const (
	writeSignalScope  = "database"
	writeSignalMetric = "blk_write_time"
)

// observeWrites reads the write signal before and after the action. A read
// error is returned for the evidence; it never fails the observation.
func (e *Engine) observeWrites(
	ctx context.Context, state WatchState,
) (Measurement, Measurement, error) {
	before, err := e.source.WriteMeasurements(
		ctx, state.Table, state.ExecutedAt.Add(-state.Window), state.ExecutedAt,
	)
	if err != nil {
		return Measurement{}, Measurement{},
			fmt.Errorf("read pre-action write measurements: %w", err)
	}
	after, err := e.source.WriteMeasurements(
		ctx, state.Table, state.ExecutedAt, e.options.Now(),
	)
	if err != nil {
		return Measurement{}, Measurement{},
			fmt.Errorf("read post-action write measurements: %w", err)
	}
	return before, after, nil
}

// writeSignal is the write_signal evidence of a verdict. exceeds_threshold
// reports a rise above verify.write_impact_pct; decisive is always false.
// Without a positive baseline (track_io_timing off, or fewer than two
// snapshots) there is no delta and the signal is unavailable.
func writeSignal(criterion Criterion, observed observations) map[string]any {
	signal := map[string]any{
		"scope": writeSignalScope, "metric": writeSignalMetric,
		"threshold_pct": criterion.WriteImpactPct, "exceeds_threshold": false,
		"decisive": false,
	}
	if observed.writeErr != nil {
		signal["error"] = observed.writeErr.Error()
		return signal
	}
	before, after := observed.writeBefore.AverageLatency, observed.writeAfter.AverageLatency
	signal["before_ms"] = toMs(before)
	signal["after_ms"] = toMs(after)
	if before <= 0 {
		signal["unavailable"] = "no pre-action write baseline " +
			"(track_io_timing off or fewer than two snapshots)"
		return signal
	}
	signal["delta_pct"] = (float64(after) - float64(before)) * 100 / float64(before)
	signal["exceeds_threshold"] = exceeds(after, before, criterion.WriteImpactPct)
	return signal
}
