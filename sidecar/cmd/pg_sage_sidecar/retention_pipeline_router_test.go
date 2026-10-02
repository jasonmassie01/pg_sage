package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/executor"
)

// The runtime adapter hands the D5 batch to the executor's pipeline: the
// action, lock timeout and row cap the pipeline chose reach the batch, and
// the batch's verified outcome comes back unchanged.
func TestRetentionDeleteAdapterCarriesRunAndOutcome(t *testing.T) {
	var got autonomy.RetentionRun
	batchErr := errors.New("batch refused")
	batch := func(_ context.Context, run autonomy.RetentionRun) (autonomy.RetentionResult,
		error) {
		got = run
		return autonomy.RetentionResult{RunID: 11, Deleted: 4, OutsidePredicate: 1,
			OutsideRelation: 2}, batchErr
	}

	outcome, err := retentionDelete(batch)(context.Background(),
		executor.RetentionExecution{ActionID: 5, LockTimeoutMS: 1500, MaxRows: 9})

	if got.ActionID != 5 || got.LockTimeout != 1500*time.Millisecond || got.MaxRows != 9 {
		t.Fatalf("batch run = %+v, want action 5, 1.5s lock timeout, 9 rows", got)
	}
	if outcome != (executor.RetentionOutcome{RunID: 11, Deleted: 4, OutsidePredicate: 1,
		OutsideRelation: 2}) || !errors.Is(err, batchErr) {
		t.Fatalf("outcome = %+v, %v; want the batch result and error unchanged", outcome, err)
	}
}

func TestRetentionDeleteAdapterWithoutBatchIsNil(t *testing.T) {
	if retentionDelete(nil) != nil {
		t.Fatal("a missing batch must stay nil so the executor refuses to admit it")
	}
}
