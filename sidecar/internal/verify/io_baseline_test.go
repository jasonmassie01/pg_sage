package verify

import (
	"math"
	"testing"
	"time"
)

// contiguousRates returns count back-to-back samples of the given interval.
func contiguousRates(count int, interval time.Duration, data, wal float64) []IORate {
	rates := make([]IORate, 0, count)
	for i := 1; i <= count; i++ {
		rates = append(rates, IORate{
			At: ioTestStart.Add(time.Duration(i) * interval), Interval: interval,
			DataBytesPerSec: data, WALBytesPerSec: wal, Source: IOSourcePGStatIO,
		})
	}
	return rates
}

func TestComputeIOBaselineEmpty(t *testing.T) {
	for name, samples := range map[string][]IORate{"nil": nil, "empty": {}} {
		if got := ComputeIOBaseline(samples); got != (IOBaseline{}) {
			t.Fatalf("%s samples baseline = %+v, want zero", name, got)
		}
	}
}

func TestComputeIOBaselineMedianOddAndEven(t *testing.T) {
	odd := contiguousRates(3, time.Minute, 0, 0)
	for i, value := range []float64{30, 10, 20} {
		odd[i].DataBytesPerSec, odd[i].WALBytesPerSec = value, value*2
	}
	got := ComputeIOBaseline(odd)
	if got.DataP50 != 20 || got.WALP50 != 40 || got.Samples != 3 {
		t.Fatalf("odd baseline = %+v, want p50 20/40", got)
	}
	even := append(odd, IORate{
		At: ioTestStart.Add(4 * time.Minute), Interval: time.Minute,
		DataBytesPerSec: 40, WALBytesPerSec: 80,
	})
	got = ComputeIOBaseline(even)
	if got.DataP50 != 25 || got.WALP50 != 50 || got.Samples != 4 {
		t.Fatalf("even baseline = %+v, want p50 25/50", got)
	}
}

func TestComputeIOBaselineObservedDaysCountsCoverage(t *testing.T) {
	// 6.9 days vs 7 days of contiguous 60-second samples.
	short := ComputeIOBaseline(contiguousRates(9936, time.Minute, 1, 1))
	full := ComputeIOBaseline(contiguousRates(10080, time.Minute, 1, 1))
	if math.Abs(short.ObservedDays-6.9) > 1e-9 {
		t.Fatalf("6.9-day observation = %v days", short.ObservedDays)
	}
	if full.ObservedDays != 7 {
		t.Fatalf("7-day observation = %v days", full.ObservedDays)
	}
}

func TestComputeIOBaselineGapsDoNotCountAsObservation(t *testing.T) {
	samples := []IORate{
		{At: ioTestStart.Add(time.Minute), Interval: time.Minute},
		// A day-long outage between samples is not observation.
		{At: ioTestStart.Add(24*time.Hour + time.Minute), Interval: time.Minute},
	}
	got := ComputeIOBaseline(samples)
	want := 2.0 / (24 * 60)
	if math.Abs(got.ObservedDays-want) > 1e-12 {
		t.Fatalf("observed days = %v, want %v", got.ObservedDays, want)
	}
}

func TestComputeIOBaselineConcurrentSamplersDoNotDoubleCount(t *testing.T) {
	first := contiguousRates(60, time.Minute, 10, 10)
	second := contiguousRates(60, time.Minute, 30, 30)
	for i := range second {
		second[i].At = second[i].At.Add(30 * time.Second)
	}
	merged := append(append([]IORate{}, second...), first...)
	got := ComputeIOBaseline(merged)
	// Two sidecars over the same hour cover one hour plus a 30 s tail.
	want := (60*time.Minute + 30*time.Second).Hours() / 24
	if math.Abs(got.ObservedDays-want) > 1e-12 || got.Samples != 120 {
		t.Fatalf("concurrent coverage = %+v, want %v days", got, want)
	}
	if got.DataP50 != 20 {
		t.Fatalf("median over both samplers = %v, want 20", got.DataP50)
	}
}
