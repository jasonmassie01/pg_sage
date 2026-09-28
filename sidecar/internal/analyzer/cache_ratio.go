package analyzer

// NormalizeCacheHitRatio converts a buffer cache hit ratio to the
// canonical unit, a fraction in [0, 1] (G2-B04/C01/G1-B08).
//
// Historically the collector emitted a percent (0-100) while every
// consumer compared against fractional thresholds. The collector now
// emits a fraction; this normalisation keeps consumers correct for
// legacy percent rows already stored in sage.snapshots and for either
// merge order of the producer/consumer fixes. Values above 1 are treated
// as percents. Negative values are the "no data" sentinel and are
// returned unchanged. RCA (rca/signals.go) should call this helper too.
func NormalizeCacheHitRatio(v float64) float64 {
	if v > 1 {
		return v / 100
	}
	return v
}
