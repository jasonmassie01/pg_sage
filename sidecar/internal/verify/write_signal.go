package verify

import (
	"context"
	"fmt"
)

// The write signal is the index's table's write cost (write_cost.go): WAL
// bytes per tuple the table wrote, before and after the build. It reverts
// as write_impact above verify.write_impact_pct, and only an index whose
// queries improved (the query reasons come first). Without attributable
// table samples it is insufficient: the queries decide, and no
// database-wide number stands in for the table (executor-03b).
const (
	writeSignalScope  = "table"
	writeSignalMetric = "wal_bytes_per_tuple"
)

// observeWrites reads the write cost before and after the action. A read
// error is returned for the evidence; it never fails the observation.
func (e *Engine) observeWrites(
	ctx context.Context, state WatchState,
) (WriteCost, WriteCost, error) {
	before, err := e.source.WriteCost(
		ctx, state.Table, state.ExecutedAt.Add(-state.Window), state.ExecutedAt,
	)
	if err != nil {
		return WriteCost{}, WriteCost{}, fmt.Errorf("read pre-action write cost: %w", err)
	}
	after, err := e.source.WriteCost(ctx, state.Table, state.ExecutedAt, e.options.Now())
	if err != nil {
		return WriteCost{}, WriteCost{}, fmt.Errorf("read post-action write cost: %w", err)
	}
	return before, after, nil
}

// judgeObservedWrites judges the observed write cost; a read error is
// insufficient evidence.
func judgeObservedWrites(criterion Criterion, observed observations) writeJudgement {
	if observed.writeErr != nil {
		return writeJudgement{Outcome: writeInsufficient,
			Reason: "the table's write cost could not be read"}
	}
	return judgeWrites(criterion.WriteImpactPct, observed.writeBefore, observed.writeAfter)
}

// writeSignal is the write_signal evidence of a verdict; decisive marks
// the signal that caused a write_impact revert.
func writeSignal(state WatchState, observed observations, decisive bool) map[string]any {
	judgement := judgeObservedWrites(state.Criterion, observed)
	signal := map[string]any{
		"scope": writeSignalScope, "metric": writeSignalMetric, "table": state.Table,
		"threshold_pct": state.Criterion.WriteImpactPct, "outcome": judgement.Outcome,
		"exceeds_threshold": judgement.Outcome == writeRegressed, "decisive": decisive,
		"before": writeCostEvidence(observed.writeBefore),
		"after":  writeCostEvidence(observed.writeAfter),
	}
	if observed.writeErr != nil {
		signal["error"] = observed.writeErr.Error()
	}
	if judgement.Measured {
		signal["delta_pct"] = judgement.DeltaPct
	} else {
		signal["insufficient"] = judgement.Reason
	}
	return signal
}

func writeCostEvidence(cost WriteCost) map[string]any {
	return map[string]any{"intervals": cost.Intervals, "unattributed": cost.Unattributed,
		"unknown": cost.Unknown, "tuples": cost.Tuples, "wal_bytes": cost.WALBytes,
		"seconds": cost.Seconds, "bytes_per_tuple": cost.BytesPerTuple}
}
