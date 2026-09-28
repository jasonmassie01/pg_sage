package providerobs

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

const MaxMetricAge = 2 * time.Minute

// Telemetry retains unknown fields as nil. CPUPct is utilization, never a counter.
// Provider disk and WAL counters are not utilization; IO admission evidence
// comes from pg-side rates instead (verify.IOMonitor).
type Telemetry struct {
	ObservedAt           time.Time
	CPUPct               *float64
	MemoryTotalBytes     *float64
	MemoryAvailableBytes *float64
}

func validAge(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now.Add(5*time.Second)) && now.Sub(at) <= MaxMetricAge
}

func validNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// CPU is the admission boundary: host CPU must be fresh and a valid percentage.
func (t Telemetry) CPU(now time.Time) (float64, error) {
	if !validAge(t.ObservedAt, now) {
		return 0, fmt.Errorf("%w: stale or future measurement",
			verify.ErrLoadTelemetryUnavailable)
	}
	if t.CPUPct == nil || !validNumber(*t.CPUPct) || *t.CPUPct > 100 {
		return 0, fmt.Errorf("%w: missing or invalid CPU utilization",
			verify.ErrLoadTelemetryUnavailable)
	}
	return *t.CPUPct, nil
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
