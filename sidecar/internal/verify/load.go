package verify

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var ErrLoadTelemetryUnavailable = errors.New(
	"host CPU and data/log I/O utilization telemetry is unavailable; " +
		"autonomous index admission is withheld; use reviewed manual execution",
)

func (e *Engine) OKToApplyNow(ctx context.Context) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{Reason: "load_unavailable"}, err
	}
	load, err := e.source.CurrentLoad(ctx)
	if err != nil {
		return Admission{Reason: "load_unavailable"}, fmt.Errorf("read current load: %w", err)
	}
	for _, value := range []float64{load.CPUPct, load.DataIOPct, load.LogIOPct} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return Admission{Reason: "load_invalid"}, errors.New(
				"load telemetry must contain finite utilization percentages in [0,100]",
			)
		}
	}
	if load.CPUPct > e.options.CPUCeilingPct {
		return Admission{Reason: "cpu_ceiling"}, nil
	}
	if load.DataIOPct > e.options.DataIOCeilingPct {
		return Admission{Reason: "data_io_ceiling"}, nil
	}
	if load.LogIOPct > e.options.LogIOCeilingPct {
		return Admission{Reason: "log_io_ceiling"}, nil
	}
	return Admission{OK: true, Reason: "load_within_ceiling"}, nil
}
