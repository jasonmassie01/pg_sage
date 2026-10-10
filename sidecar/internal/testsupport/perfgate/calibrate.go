package perfgate

import (
	"errors"
	"fmt"
	"maps"
	"math"
)

// The reference runner is the one the timing budgets were set on: a
// GitHub ubuntu-latest runner with the gate's clean PG17 container. Its
// CPU workload time is the median of three perfgate.yml runs on separate
// runners (2026-10-09: 55.2-57.3 ms; nine more that day: 42.5-65.0 ms).
// Its SQL workload time (171.7-175.7 ms on the three) was measured while
// the workload spilled to temp files: it stands until the in-memory SQL
// workload is re-measured on reference runs.
const (
	ReferenceCPUMs = 57.0
	ReferenceDBMs  = 172.2
	// MinCalibrationFactor and MaxCalibrationFactor clamp each factor. A
	// runner faster than the reference is held to budgets up to 25%
	// tighter, so its speed cannot hide a regression; a slower one gets
	// budgets up to 25% looser and no more, so a regression cannot hide
	// behind its slowness. The nine runners of 2026-10-09 timed the CPU
	// workload at x0.75-x1.14 of the reference.
	MinCalibrationFactor = 0.75
	MaxCalibrationFactor = 1.25
	calibrationRuns      = 5
)

// ErrInvalidCalibration reports a workload time that is not a positive,
// finite number of milliseconds: a broken measurement, not a speed.
var ErrInvalidCalibration = errors.New("perfgate calibration: invalid workload time")

// Calibration is the runner's speed against the reference runner: the
// best of calibrationRuns timings of each fixed workload, the factors the
// timing budgets are scaled by, and the runner's type, by which reference
// runs are grouped: its CPU model as /proc/cpuinfo names it (empty off
// Linux) and its logical CPU count.
type Calibration struct {
	CPUMs, DBMs         float64
	CPUFactor, DBFactor float64
	CPUModel            string
	CPUs                int
	Known               bool
}

// NewCalibration turns measured workload times into budget factors, each
// measured/reference clamped to MinCalibrationFactor-MaxCalibrationFactor.
// A time that is not positive and finite is an ErrInvalidCalibration.
func NewCalibration(cpuMs, dbMs float64) (Calibration, error) {
	cpu, err := calibrationFactor("CPU", cpuMs, ReferenceCPUMs)
	if err != nil {
		return Calibration{}, err
	}
	db, err := calibrationFactor("SQL", dbMs, ReferenceDBMs)
	if err != nil {
		return Calibration{}, err
	}
	return Calibration{CPUMs: cpuMs, DBMs: dbMs, CPUFactor: cpu, DBFactor: db,
		Known: true}, nil
}

func calibrationFactor(workload string, measured, reference float64) (float64, error) {
	if !(measured > 0) || math.IsInf(measured, 1) {
		return 0, fmt.Errorf("%w: %s workload took %v ms, want a positive, finite time",
			ErrInvalidCalibration, workload, measured)
	}
	return min(max(measured/reference, MinCalibrationFactor), MaxCalibrationFactor), nil
}

// EndpointFactor scales gate E: the larger of the two factors. An endpoint
// call is timed end to end in the sidecar's process, the handler's SQL on
// the server and its Go code decoding the rows and encoding the JSON, in a
// mix that differs per endpoint. The larger factor allows for whichever
// part of the runner is slower; the budget tightens only on a runner
// faster at both.
func (c Calibration) EndpointFactor() float64 {
	return max(c.CPUFactor, c.DBFactor)
}

// Calibrated returns b with its timing budgets scaled: the database times
// (statement mean and the mean exemptions' ceilings, cycle DB time,
// catalog max) by the SQL workload's factor, the sidecar's CPU by the CPU
// workload's, the endpoint by EndpointFactor. Counts are unchanged, and so
// is b: the ceilings are scaled in a copy. An unmeasured calibration
// scales nothing.
func (b Budgets) Calibrated(c Calibration) Budgets {
	if !c.Known {
		return b
	}
	b.StatementMeanMs *= c.DBFactor
	b.MeanExempt = scaledCeilings(b.MeanExempt, c.DBFactor)
	b.CycleDBTimeMs *= c.DBFactor
	b.CatalogStatementMaxMs *= c.DBFactor
	b.EndpointMaxMs *= c.EndpointFactor()
	b.SidecarCPUMsPerCycle *= c.CPUFactor
	return b
}

// scaledCeilings is a copy of exemptions with every ceiling times factor.
func scaledCeilings(exemptions map[string]MeanExemption,
	factor float64) map[string]MeanExemption {
	out := maps.Clone(exemptions)
	for tag, ex := range out {
		ex.CeilingMs *= factor
		out[tag] = ex
	}
	return out
}
