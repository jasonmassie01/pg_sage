package tuning

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// lab-04b: the exported clone measurer. Its measurements are covered on
// PostgreSQL by the migration rehearsal's integration tests
// (internal/migration/runtime/rehearsal_workload_db_test.go) and by the
// index rehearsal's, which share measurer.measure. No concurrent access
// tests: a measurer holds one connection and no shared state.

func TestNewCloneMeasurerRefusesNoConnection(t *testing.T) {
	_, err := NewCloneMeasurer(context.Background(), nil, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Fatalf("err = %v", err)
	}
}

// The exported errors are the measurer's own, so errors.Is matches what
// measure returns.
func TestCloneMeasureErrorsAreTheMeasurers(t *testing.T) {
	if !errors.Is(ErrStatementTimedOut, errAnalyzeTimedOut) ||
		!errors.Is(ErrNotOneStatement, errMultiStatement) {
		t.Fatal("exported errors differ from the measurer's")
	}
	got, err := CloneMeasurer{}.Measure(context.Background(), "SELECT 1; SELECT 2")
	if !errors.Is(err, ErrNotOneStatement) || got.Executed() || got.Plan != nil {
		t.Fatalf("multi-statement: %#v, %v", got, err)
	}
}
