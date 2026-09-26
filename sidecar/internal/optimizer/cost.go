package optimizer

import (
	"strings"
	"time"
)

// CostEstimate holds the estimated cost and benefit of an index.
type CostEstimate struct {
	EstimatedSizeBytes int64
	WriteAmplifyPct    float64
	BuildTimeEstimate  time.Duration
	QuerySavingsPerDay time.Duration
}

// EstimateIndexSize estimates index size from row count and avg
// entry bytes. Uses heuristic: size = rows * avgEntryBytes * 1.2
// for B-tree overhead.
func EstimateIndexSize(rows int64, avgEntryBytes int) int64 {
	return int64(float64(rows) * float64(avgEntryBytes) * 1.2)
}

// EstimateBuildTime estimates CONCURRENTLY build time from table
// size. Heuristic: ~1 second per 10MB, minimum 1 second.
func EstimateBuildTime(tableBytes int64) time.Duration {
	const bytesPerSecond = 10 * 1024 * 1024 // 10MB
	seconds := float64(tableBytes) / float64(bytesPerSecond)
	if seconds < 1.0 {
		return time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

// EstimateWriteAmplification computes percentage increase in
// write I/O from adding one index. Each additional index adds
// roughly 1/(existingIndexes+1) * 100 percent overhead.
func EstimateWriteAmplification(
	currentIndexCount int,
	tableWriteRate float64,
) float64 {
	if tableWriteRate <= 0 {
		return 0
	}
	return (1.0 / float64(currentIndexCount+1)) * 100.0
}

// ComputeQuerySavings estimates daily time saved from the index. Inputs
// are mean execution times in milliseconds (callers pass
// pg_stat_statements mean_exec_time), not planner cost units; the old
// 0.01 ms-per-unit factor under-stated savings 100x (G3-D10).
func ComputeQuerySavings(
	beforeMs, afterMs float64,
	callsPerDay int64,
) time.Duration {
	if afterMs >= beforeMs || callsPerDay <= 0 {
		return 0
	}
	savedPerCall := beforeMs - afterMs
	totalMs := savedPerCall * float64(callsPerDay)
	return time.Duration(totalMs * float64(time.Millisecond))
}

// avgEntryBytesForType returns a heuristic average entry size
// for the given index type.
func avgEntryBytesForType(indexType string) int {
	switch strings.ToLower(indexType) {
	case "gin":
		return 64
	case "gist":
		return 48
	case "brin":
		return 8
	case "hash":
		return 24
	default: // btree
		return 32
	}
}

// BuildCostEstimate assembles a full cost estimate for a
// recommendation using table context and optional hypoPG size.
func BuildCostEstimate(
	rec Recommendation,
	tc TableContext,
	hypoPGSizeBytes int64,
) CostEstimate {
	avgBytes := avgEntryBytesForType(rec.IndexType)

	sizeBytes := hypoPGSizeBytes
	if sizeBytes <= 0 {
		sizeBytes = EstimateIndexSize(tc.LiveTuples, avgBytes)
	}

	buildTime := EstimateBuildTime(tc.TableBytes)
	writeAmp := EstimateWriteAmplification(
		tc.IndexCount, tc.WriteRate,
	)

	var totalSavings time.Duration
	for _, q := range tc.Queries {
		saving := ComputeQuerySavings(
			q.MeanTimeMs,
			q.MeanTimeMs*(1-rec.EstimatedImprovementPct/100),
			q.Calls,
		)
		totalSavings += saving
	}

	return CostEstimate{
		EstimatedSizeBytes: sizeBytes,
		WriteAmplifyPct:    writeAmp,
		BuildTimeEstimate:  buildTime,
		QuerySavingsPerDay: totalSavings,
	}
}
