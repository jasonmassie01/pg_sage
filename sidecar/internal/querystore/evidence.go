package querystore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EvidenceStatus classifies what the query store can say about a
// queryid's latency in a window. Only EvidenceMeasured carries a latency;
// every other status is "unknown" and must never be read as "no
// regression" (G1-B26).
type EvidenceStatus string

const (
	// EvidenceMeasured: at least two monotonic samples with new calls.
	EvidenceMeasured EvidenceStatus = "measured"
	// EvidenceNotSampled: no sample in the window. The collector records
	// only the top-N statements by total time, so a target can be absent.
	EvidenceNotSampled EvidenceStatus = "not_sampled"
	// EvidenceInsufficientSamples: exactly one sample; no delta possible.
	EvidenceInsufficientSamples EvidenceStatus = "insufficient_samples"
	// EvidenceNoNewCalls: sampled, but the statement did not run.
	EvidenceNoNewCalls EvidenceStatus = "no_new_calls"
	// EvidenceCountersReset: a counter decreased inside the window
	// (pg_stat_statements reset or entry eviction); deltas are invalid.
	EvidenceCountersReset EvidenceStatus = "counters_reset"
)

// Evidence is the query store's answer for one queryid and window.
type Evidence struct {
	Status    EvidenceStatus
	Samples   int
	LatencyMs float64 // valid only when Status == EvidenceMeasured
}

// evidenceSQL reads the window endpoints and counts counter decreases
// between consecutive samples, so a reset followed by enough new calls
// to exceed the old endpoint is still detected (R10).
const evidenceSQL = `/* pg_sage */
WITH s AS (
    SELECT id, captured_at, calls, total_exec_time,
           lag(calls) OVER w AS prev_calls,
           lag(total_exec_time) OVER w AS prev_total
      FROM sage.query_store
     WHERE queryid = $1 AND captured_at >= $2 AND captured_at <= $3
    WINDOW w AS (ORDER BY captured_at, id)
)
SELECT count(*)::int,
       count(*) FILTER (WHERE calls < prev_calls OR total_exec_time < prev_total)::int,
       (array_agg(calls ORDER BY captured_at, id))[1],
       (array_agg(total_exec_time ORDER BY captured_at, id))[1],
       (array_agg(calls ORDER BY captured_at DESC, id DESC))[1],
       (array_agg(total_exec_time ORDER BY captured_at DESC, id DESC))[1]
  FROM s`

// WindowedLatencyEvidence classifies the query store evidence for a
// queryid inside [from, to] and, when measurable, returns the average
// per-call latency over the window.
func WindowedLatencyEvidence(
	ctx context.Context, pool *pgxpool.Pool, queryid int64, from, to time.Time,
) (Evidence, error) {
	var samples, decreases int
	var firstCalls, lastCalls *int64
	var firstTotal, lastTotal *float64
	err := pool.QueryRow(ctx, evidenceSQL, queryid, from, to).Scan(
		&samples, &decreases, &firstCalls, &firstTotal, &lastCalls, &lastTotal)
	if err != nil {
		return Evidence{}, fmt.Errorf("query_store evidence for %d: %w", queryid, err)
	}
	ev := Evidence{Samples: samples}
	switch {
	case samples == 0:
		ev.Status = EvidenceNotSampled
	case samples == 1:
		ev.Status = EvidenceInsufficientSamples
	case decreases > 0:
		ev.Status = EvidenceCountersReset
	case *lastCalls == *firstCalls:
		ev.Status = EvidenceNoNewCalls
	default:
		ms, ok := windowedLatencyMs(
			sampleRow{calls: *firstCalls, total: *firstTotal},
			sampleRow{calls: *lastCalls, total: *lastTotal},
		)
		if !ok {
			ev.Status = EvidenceCountersReset
			return ev, nil
		}
		ev.Status, ev.LatencyMs = EvidenceMeasured, ms
	}
	return ev, nil
}

// aggregateSamples sums samples that share a queryid, keeping first-seen
// order. The mean is recomputed from the summed totals.
func aggregateSamples(samples []Sample) []Sample {
	if len(samples) == 0 {
		return nil
	}
	index := make(map[int64]int, len(samples))
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		i, seen := index[s.QueryID]
		if !seen {
			index[s.QueryID] = len(out)
			out = append(out, s)
			continue
		}
		out[i].Calls += s.Calls
		out[i].TotalExecMs += s.TotalExecMs
		out[i].Rows += s.Rows
	}
	for i := range out {
		if out[i].Calls > 0 {
			out[i].MeanExecMs = out[i].TotalExecMs / float64(out[i].Calls)
		}
	}
	return out
}
