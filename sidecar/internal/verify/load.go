package verify

import (
	"context"
	"fmt"
)

func (e *Engine) OKToApplyNow(ctx context.Context) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{Reason: "load_unavailable"}, err
	}
	load, err := e.source.CurrentLoad(ctx)
	if err != nil {
		return Admission{Reason: "load_unavailable"}, fmt.Errorf("read current load: %w", err)
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
