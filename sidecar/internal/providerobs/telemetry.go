package providerobs

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

const MaxMetricAge = 2 * time.Minute

// Telemetry retains unknown fields as nil. Percentages are utilization, never byte counters.
type Telemetry struct {
	ObservedAt           time.Time
	CPUPct               *float64
	DataIOPct            *float64
	LogIOPct             *float64
	MemoryTotalBytes     *float64
	MemoryAvailableBytes *float64
}

func validAge(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now.Add(5*time.Second)) && now.Sub(at) <= MaxMetricAge
}

func validNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// Load is the admission boundary: every resource must have fresh, valid utilization evidence.
func (t Telemetry) Load(now time.Time) (verify.LoadSample, error) {
	if !validAge(t.ObservedAt, now) {
		return verify.LoadSample{}, fmt.Errorf("%w: stale or future measurement",
			verify.ErrLoadTelemetryUnavailable)
	}
	for _, field := range []*float64{t.CPUPct, t.DataIOPct, t.LogIOPct} {
		if field == nil || !validNumber(*field) || *field > 100 {
			return verify.LoadSample{}, fmt.Errorf("%w: missing or invalid utilization",
				verify.ErrLoadTelemetryUnavailable)
		}
	}
	return verify.LoadSample{CPUPct: *t.CPUPct, DataIOPct: *t.DataIOPct, LogIOPct: *t.LogIOPct}, nil
}

// DeriveTelemetry converts idle CPU-seconds per core to host utilization over a real interval.
// Disk and WAL byte/time counters cannot establish capacity utilization, so remain unknown.
func DeriveTelemetry(before, after MetricSnapshot, now time.Time) (Telemetry, error) {
	t := Telemetry{ObservedAt: after.ObservedAt, MemoryTotalBytes: after.MemoryTotalBytes,
		MemoryAvailableBytes: after.MemoryAvailableBytes}
	elapsed := after.ObservedAt.Sub(before.ObservedAt).Seconds()
	if !validAge(after.ObservedAt, now) || elapsed < 1 || elapsed > MaxMetricAge.Seconds() ||
		len(before.IdleCPUSeconds) == 0 || len(before.IdleCPUSeconds) != len(after.IdleCPUSeconds) {
		return t, fmt.Errorf("CPU samples require fresh, stable cores and an interval of 1-120 seconds")
	}
	idle := 0.0
	for core, value := range after.IdleCPUSeconds {
		previous, ok := before.IdleCPUSeconds[core]
		delta := value - previous
		if !ok || !validNumber(delta) || delta > elapsed {
			return t, fmt.Errorf("CPU counter reset, changed identity, or invalid interval")
		}
		idle += delta / elapsed
	}
	cpu := 100 * (1 - idle/float64(len(after.IdleCPUSeconds)))
	t.CPUPct = &cpu
	return t, nil
}
