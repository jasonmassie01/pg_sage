package rollout

import (
	"context"
	"errors"
	"time"
)

var ErrRuntimeDeferred = errors.New("fleet rollout runtime is deferred")

type RuntimeFactory func(context.Context) (*Runtime, error)

func RunPeriodic(
	ctx context.Context,
	interval time.Duration,
	factory RuntimeFactory,
	report func(error),
) {
	if interval <= 0 {
		interval = time.Hour
	}
	runScheduled(ctx, factory, report)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runScheduled(ctx, factory, report)
		}
	}
}

func runScheduled(
	ctx context.Context, factory RuntimeFactory, report func(error),
) {
	if err := ctx.Err(); err != nil {
		return
	}
	if factory == nil {
		reportRuntimeError(report, ErrRuntimeDeferred)
		return
	}
	runtime, err := factory(ctx)
	if err != nil {
		reportRuntimeError(report, err)
		return
	}
	if runtime == nil {
		reportRuntimeError(report, ErrRuntimeDeferred)
		return
	}
	_, err = runtime.RunDue(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		reportRuntimeError(report, err)
	}
}

func reportRuntimeError(report func(error), err error) {
	if report != nil {
		report(err)
	}
}
