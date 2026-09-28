package verify

import (
	"errors"
	"testing"
	"time"
)

var ioTestStart = time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)

func ioCounters(at time.Time, data, wal float64) IOCounters {
	return IOCounters{
		At: at, Source: IOSourcePGStatIO,
		DataParts: map[string]IOCounterPart{
			"client backend|relation|normal": {Bytes: data, Reset: "r1"},
		},
		WAL: IOCounterPart{Bytes: wal, Reset: "w1"},
	}
}

func TestDeriveIORateComputesBytesPerSecond(t *testing.T) {
	previous := ioCounters(ioTestStart, 1_000_000, 500_000)
	current := ioCounters(ioTestStart.Add(60*time.Second), 61_000_000, 3_500_000)
	current.DataParts["checkpointer|relation|normal"] = IOCounterPart{Bytes: 9e9, Reset: "r1"}

	rate, err := DeriveIORate(previous, current)
	if err != nil {
		t.Fatalf("DeriveIORate: %v", err)
	}
	// The part that appeared mid-interval has no baseline reading and is
	// excluded instead of being counted as 9 GB of IO in one minute.
	if rate.DataBytesPerSec != 1_000_000 || rate.WALBytesPerSec != 50_000 {
		t.Fatalf("rate = %+v, want 1e6 data and 5e4 WAL bytes/s", rate)
	}
	if rate.Interval != time.Minute || !rate.At.Equal(current.At) ||
		rate.Source != IOSourcePGStatIO {
		t.Fatalf("rate metadata = %+v", rate)
	}
}

func TestPGRateLoadRejectsResetAndShortInterval(t *testing.T) {
	previous := ioCounters(ioTestStart, 10_000, 10_000)
	later := ioTestStart.Add(time.Minute)
	resetData := ioCounters(later, 20_000, 20_000)
	resetData.DataParts["client backend|relation|normal"] = IOCounterPart{
		Bytes: 20_000, Reset: "r2",
	}
	resetWAL := ioCounters(later, 20_000, 20_000)
	resetWAL.WAL.Reset = "w2"
	tests := []struct {
		name    string
		current IOCounters
		want    error
	}{
		{"data stats_reset changed", resetData, ErrCounterReset},
		{"wal stats_reset changed", resetWAL, ErrCounterReset},
		{"data counter decreased", ioCounters(later, 5_000, 20_000), ErrCounterReset},
		{"wal counter decreased", ioCounters(later, 20_000, 5_000), ErrCounterReset},
		{"interval under 1s", ioCounters(ioTestStart.Add(999*time.Millisecond),
			20_000, 20_000), ErrSampleTooSoon},
		{"clock went backwards", ioCounters(ioTestStart.Add(-time.Minute),
			20_000, 20_000), ErrSampleTooSoon},
		{"interval over 120s", ioCounters(ioTestStart.Add(121*time.Second),
			20_000, 20_000), ErrSampleInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rate, err := DeriveIORate(previous, test.current)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if rate != (IORate{}) {
				t.Fatalf("rejected interval produced a sample: %+v", rate)
			}
		})
	}
}

func TestDeriveIORateBoundaryIntervalsAccepted(t *testing.T) {
	for _, interval := range []time.Duration{MinIOSampleInterval, MaxIOSampleInterval} {
		previous := ioCounters(ioTestStart, 0, 0)
		current := ioCounters(ioTestStart.Add(interval), 1024, 2048)
		rate, err := DeriveIORate(previous, current)
		if err != nil || rate.Interval != interval {
			t.Fatalf("interval %v: rate=%+v err=%v", interval, rate, err)
		}
	}
}

func TestDeriveIORateRejectsEmptyAndIncomparableCounters(t *testing.T) {
	later := ioTestStart.Add(time.Minute)
	empty := IOCounters{At: later, Source: IOSourcePGStatIO}
	disjoint := ioCounters(later, 5, 5)
	disjoint.DataParts = map[string]IOCounterPart{"other|relation|normal": {Bytes: 5}}
	changedSource := ioCounters(later, 20_000, 20_000)
	changedSource.Source = IOSourcePGStatDatabase
	for name, current := range map[string]IOCounters{
		"nil data parts":         empty,
		"no shared parts":        disjoint,
		"counter source changed": changedSource,
	} {
		t.Run(name, func(t *testing.T) {
			rate, err := DeriveIORate(ioCounters(ioTestStart, 1, 1), current)
			if !errors.Is(err, ErrCounterReset) || rate != (IORate{}) {
				t.Fatalf("rate=%+v err=%v, want ErrCounterReset", rate, err)
			}
		})
	}
	rate, err := DeriveIORate(IOCounters{}, ioCounters(later, 1, 1))
	if err == nil || rate != (IORate{}) {
		t.Fatalf("zero previous reading produced rate %+v", rate)
	}
}

func TestDeriveIORateIgnoresDroppedDatabase(t *testing.T) {
	previous := ioCounters(ioTestStart, 1000, 0)
	previous.DataParts["db:99"] = IOCounterPart{Bytes: 1e12}
	current := ioCounters(ioTestStart.Add(10*time.Second), 11_000, 0)
	rate, err := DeriveIORate(previous, current)
	if err != nil || rate.DataBytesPerSec != 1000 {
		t.Fatalf("dropped database: rate=%+v err=%v", rate, err)
	}
}
