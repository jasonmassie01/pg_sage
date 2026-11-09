package forecaster

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// defaultCheckpointRequestedPerHour is the config default of
// forecaster.checkpoint_requested_per_hour, used when a caller leaves the
// threshold unset (config validation rejects 0 and below).
const defaultCheckpointRequestedPerHour = 2

// checkpointInterval is the counter growth between the last split samples
// of two usable days.
type checkpointInterval struct {
	hours, timed, requested float64
}

// checkpointIntervals pairs each closed day that carries the split
// counters with the previous one (forecast-03). The rate spans the hours
// between the two days' last samples, so a missing day does not inflate
// it. A counter drop (a stats reset) skips the interval rather than
// reading as 0, and the reset day is the next baseline; a day whose
// sample time does not move forward is skipped.
func checkpointIntervals(aggs []DaySystemAgg) []checkpointInterval {
	var out []checkpointInterval
	var prev *DaySystemAgg
	for i := range aggs {
		a := &aggs[i]
		if a.CheckpointsUnknown || a.CheckpointsLastAt.IsZero() {
			continue
		}
		if prev != nil {
			iv := checkpointInterval{
				hours:     a.CheckpointsLastAt.Sub(prev.CheckpointsLastAt).Hours(),
				timed:     a.CheckpointsTimed - prev.CheckpointsTimed,
				requested: a.CheckpointsRequested - prev.CheckpointsRequested,
			}
			if iv.hours <= 0 {
				continue
			}
			if iv.timed >= 0 && iv.requested >= 0 {
				out = append(out, iv)
			}
		}
		prev = a
	}
	return out
}

// checkpointEvaluable reports whether the history holds minDataPoints
// usable intervals. Without them (legacy rows lack the split, a fresh
// install, a reset) the category is not evaluated, so an open finding is
// neither refreshed nor resolved.
func checkpointEvaluable(aggs []DaySystemAgg) bool {
	return len(checkpointIntervals(aggs)) >= minDataPoints
}

func checkpointRequestedLimit(cfg ForecasterConfig) float64 {
	v := cfg.CheckpointRequestedPerHour
	if !(v > 0) || math.IsInf(v, 1) {
		return defaultCheckpointRequestedPerHour
	}
	return v
}

// forecastCheckpointPressure warns when the EWMA of requested
// (WAL-triggered or explicit) checkpoints per hour reaches
// forecaster.checkpoint_requested_per_hour. Timed checkpoints are not
// pressure: they are counted every checkpoint_timeout even when skipped.
func forecastCheckpointPressure(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	intervals := checkpointIntervals(aggs)
	if len(intervals) < minDataPoints {
		return nil
	}
	rates := make([]float64, len(intervals))
	var total checkpointInterval
	for i, iv := range intervals {
		rates[i] = iv.requested / iv.hours
		total.hours += iv.hours
		total.timed += iv.timed
		total.requested += iv.requested
	}
	limit := checkpointRequestedLimit(cfg)
	smoothed := EWMA(rates, 0.1)
	if smoothed < limit*(1-1e-9) { // a rate of exactly the limit warns
		return nil
	}
	return []analyzer.Finding{checkpointFinding(total, rates, smoothed, limit)}
}

func checkpointFinding(total checkpointInterval, rates []float64,
	smoothed, limit float64) analyzer.Finding {
	share := 0.0
	if all := total.timed + total.requested; all > 0 {
		share = total.requested / all
	}
	return analyzer.Finding{
		Category:   "forecast_checkpoint_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title:      fmt.Sprintf("Requested checkpoints at %.1f/hr", smoothed),
		Detail: map[string]any{
			"forecast_type":           "checkpoint_pressure",
			"ewma_requested_per_hour": smoothed,
			"threshold_per_hour":      limit,
			"requested_share":         share,
			"checkpoints_requested":   total.requested,
			"checkpoints_timed":       total.timed,
			"window_hours":            total.hours,
			"hourly_rates":            rates,
		},
		Recommendation: fmt.Sprintf(
			"%.0f requested checkpoints in the last %.0f hours (%.1f/hr, "+
				"%.0f%% of all checkpoints). A requested checkpoint starts when "+
				"WAL reaches max_wal_size before checkpoint_timeout, or on an "+
				"explicit CHECKPOINT or base backup. Raise max_wal_size so the WAL "+
				"written in one checkpoint_timeout fits; with advisor.enabled, "+
				"pg_sage's configuration advisor reviews max_wal_size "+
				"(wal_tuning findings).",
			total.requested, total.hours, smoothed, share*100),
	}
}
