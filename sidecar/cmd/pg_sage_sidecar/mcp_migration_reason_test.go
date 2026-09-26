package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

type stepFailingRehearser struct{ err error }

func (r stepFailingRehearser) Rehearse(
	context.Context, plan.Plan,
) (rehearsal.Result, error) {
	return rehearsal.Result{}, r.err
}

// TestRehearsalStepFailureIsNotCloneUnavailable is the G7-B25
// regression: a genuine DDL failure on the clone (e.g. duplicate keys
// for a unique index) was reported as clone_unavailable.
func TestRehearsalStepFailureIsNotCloneUnavailable(t *testing.T) {
	stepErr := fmt.Errorf("%w: %w", rehearsal.ErrStepFailed,
		&pgconn.PgError{Code: "23505", Message: "duplicate key"})
	wrapper := recommendOnlyOnCloneFailure{delegate: stepFailingRehearser{stepErr}}
	result, err := wrapper.Rehearse(context.Background(), plan.Plan{})
	require.NoError(t, err)
	require.Equal(t, rehearsal.VerdictRecommendOnly, result.Verdict)
	require.Equal(t, rehearsal.Reason("rehearsal_failed:23505"), result.Reason)

	cloneErr := fmt.Errorf("%w: no capacity", rehearsal.ErrCloneUnavailable)
	wrapper = recommendOnlyOnCloneFailure{delegate: stepFailingRehearser{cloneErr}}
	result, err = wrapper.Rehearse(context.Background(), plan.Plan{})
	require.NoError(t, err)
	require.Equal(t, rehearsal.Reason("clone_unavailable"), result.Reason)
}
