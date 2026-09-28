package verify

import (
	"context"
	"errors"
	"fmt"
)

var ErrLoadTelemetryUnavailable = errors.New(
	"host CPU and data/log I/O utilization telemetry is unavailable; " +
		"autonomous index admission is withheld; use reviewed manual execution",
)

// OKToApplyNow decides whether an autonomous index build may start now.
// Any failure to gather evidence fails closed as load_unavailable.
func (e *Engine) OKToApplyNow(ctx context.Context) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return unavailableAdmission(err.Error()), err
	}
	evidence, err := e.source.LoadEvidence(ctx)
	if err != nil {
		return unavailableAdmission(err.Error()), fmt.Errorf("read current load: %w", err)
	}
	admission := DecideAdmission(evidence, e.options)
	if admission.Reason == ReasonLoadInvalid {
		return admission, errors.New(
			"load telemetry must contain finite utilization percentages in [0,100]",
		)
	}
	return admission, nil
}
