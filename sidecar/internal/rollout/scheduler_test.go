package rollout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestPeriodicSchedulerFailsClosedWithoutFactoryAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var reported error

	RunPeriodic(ctx, time.Hour, nil, func(err error) {
		reported = err
		cancel()
	})

	require.ErrorIs(t, reported, ErrRuntimeDeferred)
}

func TestPeriodicSchedulerActivatesSuppliedRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRuntimeEngine{run: func(
		context.Context, Request,
	) (Result, error) {
		cancel()
		return Result{Instances: map[string]InstanceResult{}}, nil
	}}
	runtime := newTestRuntime(
		runner,
		&fakeRuntimeEvidenceSource{items: []PriorEvidence{
			priorEvidence("prior", "success", time.Now()),
		}},
		runtimeInstances("db-a"), fixedRuntimePolicy(1, 1, 10), time.Now(),
	)
	factory := func(context.Context) (*Runtime, error) {
		return runtime, nil
	}

	RunPeriodic(ctx, time.Hour, factory, func(err error) {
		t.Fatalf("unexpected scheduler error: %v", err)
	})

	require.Len(t, runner.requests, 1)
}

func TestPeriodicSchedulerReportsFactoryFailureWithoutMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	want := errors.New("backend unavailable")
	var reported error
	factory := func(context.Context) (*Runtime, error) {
		return nil, want
	}

	RunPeriodic(ctx, time.Millisecond, factory, func(err error) {
		reported = err
		cancel()
	})

	require.ErrorIs(t, reported, want)
}
