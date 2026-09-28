package verify

import (
	"slices"
	"time"
)

// IOBaseline summarizes the rolling window of persisted IO rates.
// ObservedDays counts only time actually covered by samples, so gaps while
// the sidecar was down do not count as observation and overlapping samples
// from concurrent samplers are not counted twice.
type IOBaseline struct {
	ObservedDays float64
	Samples      int
	DataP50      float64
	WALP50       float64
}

// ComputeIOBaseline derives the observed coverage and median rates.
func ComputeIOBaseline(samples []IORate) IOBaseline {
	if len(samples) == 0 {
		return IOBaseline{}
	}
	data := make([]float64, 0, len(samples))
	wal := make([]float64, 0, len(samples))
	for _, sample := range samples {
		data = append(data, sample.DataBytesPerSec)
		wal = append(wal, sample.WALBytesPerSec)
	}
	return IOBaseline{
		ObservedDays: observedCoverage(samples).Hours() / 24,
		Samples:      len(samples), DataP50: median(data), WALP50: median(wal),
	}
}

// observedCoverage is the length of the union of the sampled intervals
// [At-Interval, At].
func observedCoverage(samples []IORate) time.Duration {
	ordered := slices.Clone(samples)
	slices.SortFunc(ordered, func(a, b IORate) int {
		return a.At.Add(-a.Interval).Compare(b.At.Add(-b.Interval))
	})
	var covered time.Duration
	var spanStart, spanEnd time.Time
	for i, sample := range ordered {
		start, end := sample.At.Add(-sample.Interval), sample.At
		if i > 0 && !start.After(spanEnd) {
			if end.After(spanEnd) {
				spanEnd = end
			}
			continue
		}
		covered += spanEnd.Sub(spanStart)
		spanStart, spanEnd = start, end
	}
	return covered + spanEnd.Sub(spanStart)
}

func median(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
